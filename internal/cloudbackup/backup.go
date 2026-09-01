package cloudbackup

import (
	"context"
	"errors"
	"fmt"
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
		result := model.SourceBackupResult{SourceID: source.ID}
		started := time.Now().UTC()
		root, err := s.store.prepareSource(source.ID)
		if err == nil {
			sourceCtx, cancel := context.WithTimeout(ctx, source.Timeout.Duration)
			err = s.rclone.Copy(sourceCtx, source, root)
			cancel()
		}
		if err == nil {
			previous := map[string]model.File{}
			err = s.catalog.ForEachFile(ctx, source.ID, func(file model.File) error {
				previous[file.Path] = file
				return nil
			})
			var manifest model.Manifest
			if err == nil {
				manifest, result, err = s.store.scan(ctx, source.ID, previous)
				manifest.RunID = runID
				manifest.Remote = source.Remote
				manifest.StartedAt = started
				manifest.CompletedAt = time.Now().UTC()
			}
			if err == nil {
				result.Manifest, err = s.store.writeManifest(manifest)
			}
			if err == nil {
				err = s.catalog.ApplyManifest(ctx, manifest)
			}
		}
		if err != nil {
			result.Error = err.Error()
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
