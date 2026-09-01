package cloudbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lauritsk/backup/internal/atomicfile"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

type fileStore struct{ dataDir string }

func newFileStore(dataDir string) (*fileStore, error) {
	for _, dir := range []string{filepath.Join(dataDir, "sources"), filepath.Join(dataDir, "restores")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("storage path %s is not a directory", dir)
		}
	}
	return &fileStore{dataDir: dataDir}, nil
}
func (s *fileStore) sourceRoot(id string) string { return filepath.Join(s.dataDir, "sources", id) }
func (s *fileStore) filesRoot(id string) string  { return filepath.Join(s.sourceRoot(id), "files") }
func (s *fileStore) manifestsRoot(id string) string {
	return filepath.Join(s.sourceRoot(id), "manifests")
}
func (s *fileStore) prepareSource(id string) (string, error) {
	root := s.sourceRoot(id)
	if info, err := os.Lstat(root); err == nil && !info.IsDir() {
		return "", fmt.Errorf("source path %s is not a directory", root)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, dir := range []string{s.filesRoot(id), s.manifestsRoot(id)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	return s.filesRoot(id), nil
}

func (s *fileStore) scan(ctx context.Context, sourceID string, previous map[string]model.File) (model.Manifest, model.SourceBackupResult, error) {
	result := model.SourceBackupResult{SourceID: sourceID}
	manifest := model.Manifest{SchemaVersion: 1, SourceID: sourceID}
	root := s.filesRoot(sourceID)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed beneath source files: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular source file: %s", name)
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		digest, err := hashFile(ctx, name)
		if err != nil {
			return err
		}
		status := "added"
		if old, found := previous[rel]; found {
			if old.Size == info.Size() && old.SHA256 == digest {
				status = "skipped"
				result.Skipped++
			} else {
				status = "changed"
				result.Changed++
			}
		} else {
			result.Added++
		}
		manifest.Files = append(manifest.Files, model.ManifestFile{Path: rel, Size: info.Size(), SHA256: digest, ModTime: info.ModTime().UTC(), Status: status})
		result.Files++
		result.Bytes += info.Size()
		return nil
	})
	if err != nil {
		return manifest, result, err
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	return manifest, result, nil
}

func hashFile(ctx context.Context, name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func (s *fileStore) writeManifest(manifest model.Manifest) (string, error) {
	latest := filepath.Join(s.manifestsRoot(manifest.SourceID), "latest.json")
	if err := writeManifestFile(latest, manifest); err != nil {
		return "", err
	}
	name := filepath.Join(s.manifestsRoot(manifest.SourceID), manifest.RunID+".json")
	if err := writeManifestFile(name, manifest); err != nil {
		return "", err
	}
	return name, nil
}

func writeManifestFile(name string, manifest model.Manifest) error {
	return atomicfile.Write(name, 0o600, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(manifest)
	})
}
func (s *fileStore) readManifests(ctx context.Context) ([]model.Manifest, error) {
	var result []model.Manifest
	root := filepath.Join(s.dataDir, "sources")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, source := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !source.IsDir() {
			continue
		}
		dir := filepath.Join(root, source.Name(), "manifests")
		files, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range files {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == "latest.json" {
				continue
			}
			name := filepath.Join(dir, entry.Name())
			manifest, err := readManifestFile(name, source.Name())
			if err != nil {
				return nil, err
			}
			result = append(result, manifest)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CompletedAt.Before(result[j].CompletedAt) })
	return result, nil
}

func (s *fileStore) readLatestManifests(ctx context.Context) ([]model.Manifest, error) {
	root := filepath.Join(s.dataDir, "sources")
	sources, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var result []model.Manifest
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !source.IsDir() {
			continue
		}
		name := filepath.Join(root, source.Name(), "manifests", "latest.json")
		manifest, err := readManifestFile(name, source.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, manifest)
	}
	return result, nil
}

func readManifestFile(name, sourceID string) (model.Manifest, error) {
	file, err := os.Open(name)
	if err != nil {
		return model.Manifest{}, err
	}
	var manifest model.Manifest
	decodeErr := json.NewDecoder(io.LimitReader(file, 32<<20)).Decode(&manifest)
	closeErr := file.Close()
	if decodeErr != nil {
		return model.Manifest{}, fmt.Errorf("decode manifest %s: %w", name, decodeErr)
	}
	if closeErr != nil {
		return model.Manifest{}, closeErr
	}
	if manifest.SchemaVersion != 1 || manifest.SourceID != sourceID {
		return model.Manifest{}, fmt.Errorf("invalid manifest %s", name)
	}
	return manifest, nil
}

func cleanRelative(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", errors.New("path must be a non-empty relative slash path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", errors.New("path is not a clean relative path")
	}
	return clean, nil
}
func (s *fileStore) open(sourceID, path string) (*os.File, error) {
	clean, err := cleanRelative(path)
	if err != nil {
		return nil, err
	}
	root := s.filesRoot(sourceID)
	name := filepath.Join(root, filepath.FromSlash(clean))
	if err := rejectSymlinkPath(root, name); err != nil {
		return nil, err
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("backed-up path is not a regular file")
	}
	return file, nil
}
func rejectSymlinkPath(root, name string) error {
	rel, err := filepath.Rel(root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes source root")
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink path is not allowed")
		}
	}
	return nil
}
func (s *fileStore) verify(ctx context.Context, file model.File) error {
	opened, err := s.open(file.SourceID, file.Path)
	if err != nil {
		return err
	}
	info, err := opened.Stat()
	if err != nil {
		opened.Close()
		return err
	}
	if info.Size() != file.Size {
		opened.Close()
		return fmt.Errorf("size is %d, manifest records %d", info.Size(), file.Size)
	}
	hash := sha256.New()
	_, copyErr := copyContext(ctx, hash, opened)
	closeErr := opened.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != file.SHA256 {
		return fmt.Errorf("SHA-256 mismatch: got %s", got)
	}
	return nil
}
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buffer)
		if n > 0 {
			written, werr := dst.Write(buffer[:n])
			total += int64(written)
			if werr != nil {
				return total, werr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(rerr, io.EOF) {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
func (s *fileStore) restore(ctx context.Context, runID string, file model.File) (model.RestoredFile, error) {
	entry := model.RestoredFile{SourceID: file.SourceID, Path: file.Path}
	source, err := s.open(file.SourceID, file.Path)
	if err != nil {
		return entry, err
	}
	defer source.Close()
	output := filepath.Join(s.dataDir, "restores", runID, file.SourceID, filepath.FromSlash(file.Path))
	err = atomicfile.Write(output, 0o600, func(writer io.Writer) error {
		size, err := copyContext(ctx, writer, source)
		entry.Size = size
		return err
	})
	if err != nil {
		return entry, err
	}
	entry.OutputPath = output
	return entry, nil
}
