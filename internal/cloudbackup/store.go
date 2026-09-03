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
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lauritsk/backup/internal/atomicfile"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

type fileStore struct {
	dataDir string
	root    *os.Root
}

func newFileStore(dataDir string) (*fileStore, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, fmt.Errorf("open cloud data root: %w", err)
	}
	store := &fileStore{dataDir: dataDir, root: root}
	for _, dir := range []string{"sources", "restores"} {
		if err := store.ensureDirectories(dir); err != nil {
			root.Close()
			return nil, err
		}
	}
	return store, nil
}

func (s *fileStore) Close() error                { return s.root.Close() }
func (s *fileStore) sourceRoot(id string) string { return filepath.Join(s.dataDir, "sources", id) }
func (s *fileStore) filesRoot(id string) string  { return filepath.Join(s.sourceRoot(id), "files") }
func (s *fileStore) manifestsRoot(id string) string {
	return filepath.Join(s.sourceRoot(id), "manifests")
}
func (s *fileStore) sourcePath(id, suffix string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\\\x00`) || id == "." || id == ".." {
		return "", errors.New("invalid cloud source ID")
	}
	return path.Join("sources", id, suffix), nil
}
func (s *fileStore) prepareSource(id string) (string, error) {
	for _, suffix := range []string{"files", "manifests"} {
		dir, err := s.sourcePath(id, suffix)
		if err != nil {
			return "", err
		}
		if err := s.ensureDirectories(dir); err != nil {
			return "", err
		}
	}
	return s.filesRoot(id), nil
}

func (s *fileStore) ensureDirectories(name string) error {
	if !fs.ValidPath(name) {
		return errors.New("directory escapes the cloud data root")
	}
	current := ""
	for _, part := range strings.Split(name, "/") {
		if current == "" {
			current = part
		} else {
			current = path.Join(current, part)
		}
		info, err := s.root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := s.root.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = s.root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsDir() {
			return fmt.Errorf("cloud storage path %s is not a plain directory", filepath.Join(s.dataDir, filepath.FromSlash(current)))
		}
		if err := s.root.Chmod(current, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (s *fileStore) commit(ctx context.Context, sourceID string, remote model.RemoteFile, download func(io.Writer) error) (model.ManifestFile, error) {
	clean, err := cleanRelative(remote.Path)
	if err != nil {
		return model.ManifestFile{}, fmt.Errorf("invalid remote path %q: %w", remote.Path, err)
	}
	files, err := s.sourcePath(sourceID, "files")
	if err != nil {
		return model.ManifestFile{}, err
	}
	target := path.Join(files, clean)
	if err := s.ensureDirectories(path.Dir(target)); err != nil {
		return model.ManifestFile{}, err
	}
	hash := sha256.New()
	var size int64
	var digest string
	err = atomicfile.WriteRoot(s.root, target, 0o600, func(writer io.Writer) error {
		counter := &countWriter{writer: io.MultiWriter(writer, hash)}
		if err := download(counter); err != nil {
			return err
		}
		size = counter.count
		if remote.Size >= 0 && size != remote.Size {
			return fmt.Errorf("remote file size is %d, inventory reported %d", size, remote.Size)
		}
		digest = hex.EncodeToString(hash.Sum(nil))
		if expected := remoteSHA256(remote); expected != "" && !strings.EqualFold(expected, digest) {
			return fmt.Errorf("remote SHA-256 mismatch: got %s", digest)
		}
		return ctx.Err()
	})
	if err != nil {
		return model.ManifestFile{}, err
	}
	return model.ManifestFile{Path: clean, Size: size, SHA256: digest, ModTime: remote.ModTime.UTC()}, nil
}

type countWriter struct {
	writer io.Writer
	count  int64
}

func (w *countWriter) Write(value []byte) (int, error) {
	n, err := w.writer.Write(value)
	w.count += int64(n)
	return n, err
}

func remoteSHA256(remote model.RemoteFile) string {
	for key, value := range remote.Hashes {
		normalized := strings.ToLower(strings.ReplaceAll(key, "-", ""))
		if normalized == "sha256" && len(value) == sha256.Size*2 {
			return strings.ToLower(value)
		}
	}
	return ""
}

func (s *fileStore) readable(file model.File) bool {
	opened, err := s.open(file.SourceID, file.Path)
	if err != nil {
		return false
	}
	defer opened.Close()
	info, err := opened.Stat()
	return err == nil && info.Size() == file.Size
}

func (s *fileStore) writeManifest(manifest model.Manifest) (string, error) {
	dir, err := s.sourcePath(manifest.SourceID, "manifests")
	if err != nil {
		return "", err
	}
	if err := s.ensureDirectories(dir); err != nil {
		return "", err
	}
	history := path.Join(dir, manifest.RunID+".json")
	if err := s.writeManifestFile(history, manifest); err != nil {
		return "", err
	}
	if err := s.writeManifestFile(path.Join(dir, "latest.json"), manifest); err != nil {
		return "", err
	}
	return filepath.Join(s.dataDir, filepath.FromSlash(history)), nil
}
func (s *fileStore) writeManifestFile(name string, manifest model.Manifest) error {
	return atomicfile.WriteRoot(s.root, name, 0o600, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(manifest)
	})
}

func (s *fileStore) readManifests(ctx context.Context) ([]model.Manifest, error) {
	return s.readManifestSet(ctx, false)
}
func (s *fileStore) readLatestManifests(ctx context.Context) ([]model.Manifest, error) {
	return s.readManifestSet(ctx, true)
}
func (s *fileStore) readManifestSet(ctx context.Context, latestOnly bool) ([]model.Manifest, error) {
	sources, err := fs.ReadDir(s.root.FS(), "sources")
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
		dir := path.Join("sources", source.Name(), "manifests")
		if latestOnly {
			manifest, err := s.readManifestFile(path.Join(dir, "latest.json"), source.Name())
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result = append(result, manifest)
			continue
		}
		entries, err := fs.ReadDir(s.root.FS(), dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || path.Ext(entry.Name()) != ".json" || entry.Name() == "latest.json" {
				continue
			}
			manifest, err := s.readManifestFile(path.Join(dir, entry.Name()), source.Name())
			if err != nil {
				return nil, err
			}
			result = append(result, manifest)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CompletedAt.Before(result[j].CompletedAt) })
	return result, nil
}
func (s *fileStore) readManifestFile(name, sourceID string) (model.Manifest, error) {
	file, err := s.openRegular(name)
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
	if value == "" || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || !fs.ValidPath(value) {
		return "", errors.New("path must be a clean relative slash path")
	}
	return value, nil
}
func (s *fileStore) rejectSymlinks(name string) error {
	current := ""
	for _, part := range strings.Split(name, "/") {
		if current == "" {
			current = part
		} else {
			current = path.Join(current, part)
		}
		info, err := s.root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink path is not allowed")
		}
	}
	return nil
}
func (s *fileStore) openRegular(name string) (*os.File, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, errors.New("invalid rooted file path")
	}
	if err := s.rejectSymlinks(name); err != nil {
		return nil, err
	}
	file, err := s.root.Open(name)
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
		return nil, errors.New("stored path is not a regular file")
	}
	return file, nil
}
func (s *fileStore) open(sourceID, filePath string) (*os.File, error) {
	clean, err := cleanRelative(filePath)
	if err != nil {
		return nil, err
	}
	files, err := s.sourcePath(sourceID, "files")
	if err != nil {
		return nil, err
	}
	return s.openRegular(path.Join(files, clean))
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
		n, readErr := src.Read(buffer)
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
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
	clean, err := cleanRelative(file.Path)
	if err != nil {
		return entry, err
	}
	output := path.Join("restores", runID, file.SourceID, clean)
	if err := s.ensureDirectories(path.Dir(output)); err != nil {
		return entry, err
	}
	err = atomicfile.WriteRoot(s.root, output, 0o600, func(writer io.Writer) error {
		size, err := copyContext(ctx, writer, source)
		entry.Size = size
		return err
	})
	if err != nil {
		return entry, err
	}
	entry.OutputPath = filepath.Join(s.dataDir, filepath.FromSlash(output))
	return entry, nil
}
