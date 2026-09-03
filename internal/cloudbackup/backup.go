package cloudbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func (s *Service) backup(ctx context.Context, runID string, request model.BackupRequest) (model.BackupReport, error) {
	sources, err := s.selectedSources(request.Sources)
	if err != nil {
		return model.BackupReport{}, err
	}
	if len(sources) == 0 {
		return model.BackupReport{}, errors.New("no enabled cloud sources are configured")
	}
	report := model.BackupReport{}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result := s.backupSource(ctx, runID, source)
		if result.Error != "" {
			report.Errors++
		}
		report.Sources = append(report.Sources, result)
		report.Added += result.Added
		report.Changed += result.Changed
		report.Skipped += result.Skipped
		report.Files += result.Files
		report.Bytes += result.Bytes
	}
	if report.Errors > 0 {
		return report, fmt.Errorf("backup completed with %d error(s)", report.Errors)
	}
	return report, nil
}

func (s *Service) backupSource(ctx context.Context, runID string, source config.SourceConfig) (result model.SourceBackupResult) {
	result.SourceID = source.ID
	started := time.Now().UTC()
	if _, err := s.store.prepareSource(source.ID); err != nil {
		result.Error = err.Error()
		return
	}
	previous := make(map[string]model.File)
	if err := s.catalog.ForEachFile(ctx, source.ID, func(file model.File) error {
		previous[file.Path] = file
		return nil
	}); err != nil {
		result.Error = err.Error()
		return
	}

	sourceCtx, cancel := context.WithTimeout(ctx, source.Timeout.Duration)
	defer cancel()
	inventory, err := s.rclone.Inventory(sourceCtx, source)
	if err != nil {
		result.Error = err.Error()
		return
	}
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Path < inventory[j].Path })
	manifest := model.Manifest{SchemaVersion: 1, RunID: runID, SourceID: source.ID, Remote: source.Remote, StartedAt: started}
	seen := make(map[string]struct{}, len(inventory))
	localNames := make(map[string]string, len(inventory))
	for _, remote := range inventory {
		if err := sourceCtx.Err(); err != nil {
			result.Error = err.Error()
			return
		}
		if remote.IsDir {
			continue
		}
		clean, err := cleanRelative(remote.Path)
		if err != nil {
			result.Error = fmt.Sprintf("remote object %q: %v", remote.Path, err)
			return
		}
		if _, duplicate := seen[clean]; duplicate {
			result.Error = fmt.Sprintf("remote inventory contains duplicate path %q", clean)
			return
		}
		folded := strings.ToLower(clean)
		if other, collision := localNames[folded]; collision {
			result.Error = fmt.Sprintf("remote paths %q and %q collide on case-insensitive filesystems", other, clean)
			return
		}
		seen[clean] = struct{}{}
		localNames[folded] = clean
		remote.Path = clean
		old, existed := previous[clean]
		expectedHash := remoteSHA256(remote)
		if existed && expectedHash != "" && old.Size == remote.Size && old.SHA256 == expectedHash && s.store.readable(old) {
			manifest.Files = append(manifest.Files, model.ManifestFile{Path: clean, Size: old.Size, SHA256: old.SHA256, ModTime: remote.ModTime.UTC(), Status: "skipped"})
			result.Skipped++
		} else {
			file, err := s.store.commit(sourceCtx, source.ID, remote, func(writer io.Writer) error {
				return s.rclone.Download(sourceCtx, source, clean, writer)
			})
			if err != nil {
				result.Error = fmt.Sprintf("acquire remote object %q: %v", clean, err)
				return
			}
			if existed {
				file.Status = "changed"
			} else {
				file.Status = "added"
			}
			if err := s.catalog.ApplyFile(sourceCtx, runID, source.ID, source.Remote, file); err != nil {
				result.Error = fmt.Sprintf("catalog durable remote object %q: %v", clean, err)
				return
			}
			if existed {
				result.Changed++
			} else {
				result.Added++
			}
			manifest.Files = append(manifest.Files, file)
		}
	}
	// Cloud Backup retains objects deleted at the source. Keep them in every
	// manifest so catalog rebuild does not lose the retained local copy.
	for filePath, old := range previous {
		if _, found := seen[filePath]; found {
			continue
		}
		if !s.store.readable(old) {
			result.Error = fmt.Sprintf("retained local object %q is missing or unreadable", filePath)
			return
		}
		manifest.Files = append(manifest.Files, model.ManifestFile{Path: old.Path, Size: old.Size, SHA256: old.SHA256, ModTime: old.ModTime, Status: "skipped"})
		result.Skipped++
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	for _, file := range manifest.Files {
		result.Files++
		result.Bytes += file.Size
	}
	manifest.CompletedAt = time.Now().UTC()
	result.Manifest, err = s.store.writeManifest(manifest)
	if err == nil {
		err = s.catalog.ApplyManifest(ctx, manifest)
	}
	if err != nil {
		result.Error = err.Error()
	}
	return
}

func (s *Service) selectedSources(ids []string) ([]config.SourceConfig, error) {
	if len(ids) == 0 {
		return s.config.EnabledSources(), nil
	}
	seen := map[string]bool{}
	var result []config.SourceConfig
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		source, found := s.config.Source(id)
		if !found {
			return nil, fmt.Errorf("source %q is not configured", id)
		}
		if source.Disabled {
			return nil, fmt.Errorf("source %q is disabled", id)
		}
		result = append(result, source)
	}
	return result, nil
}
