package appbackup

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
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/atomicfile"
)

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type fileStore struct {
	dataDir string
	root    *os.Root
}

func newFileStore(dataDir string) (*fileStore, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, err
	}
	store := &fileStore{dataDir: dataDir, root: root}
	for _, name := range []string{"recovery-points", "staging", "exports", "restic"} {
		if err := store.secureDirectory(name); err != nil {
			root.Close()
			return nil, err
		}
	}
	return store, nil
}
func (s *fileStore) Close() error { return s.root.Close() }
func (s *fileStore) secureDirectory(name string) error {
	if !fs.ValidPath(name) {
		return errors.New("directory escapes the data root")
	}
	current := ""
	for _, part := range strings.Split(name, "/") {
		current = pathpkg.Join(current, part)
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
			return fmt.Errorf("%s is not a plain directory", filepath.Join(s.dataDir, filepath.FromSlash(current)))
		}
		if err := s.root.Chmod(current, 0o700); err != nil {
			return err
		}
	}
	return nil
}
func (s *fileStore) relative(absolute string) (string, error) {
	relative, err := filepath.Rel(s.dataDir, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the data root")
	}
	result := filepath.ToSlash(relative)
	if !fs.ValidPath(result) {
		return "", errors.New("invalid data path")
	}
	return result, nil
}
func (s *fileStore) recoveryRoot() string { return filepath.Join(s.dataDir, "recovery-points") }
func (s *fileStore) pointDir(id string) (string, error) {
	if !safeID.MatchString(id) {
		return "", errors.New("invalid recovery point ID")
	}
	return filepath.Join(s.recoveryRoot(), id), nil
}
func (s *fileStore) manifestPath(id string) (string, error) {
	dir, err := s.pointDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "manifest.json"), nil
}
func (s *fileStore) stagingDir(id, app string) (string, error) {
	if !safeID.MatchString(id) || !safeID.MatchString(app) {
		return "", errors.New("invalid staging ID")
	}
	absolute := filepath.Join(s.dataDir, "staging", id, app)
	relative, err := s.relative(absolute)
	if err != nil {
		return "", err
	}
	return absolute, s.secureDirectory(relative)
}
func (s *fileStore) prepareDumpDir(staging string) error {
	relative, err := s.relative(filepath.Join(staging, "dumps"))
	if err != nil {
		return err
	}
	return s.secureDirectory(relative)
}
func (s *fileStore) exportDir(runID, pointID string) (string, error) {
	if !safeID.MatchString(runID) || !safeID.MatchString(pointID) {
		return "", errors.New("invalid recovery point ID")
	}
	absolute := filepath.Join(s.dataDir, "exports", runID, pointID)
	relative, err := s.relative(absolute)
	if err != nil {
		return "", err
	}
	return absolute, s.secureDirectory(relative)
}

func (s *fileStore) writeRecoveryPoint(point model.RecoveryPoint) (string, error) {
	if err := s.validateRecoveryPoint(point); err != nil {
		return "", err
	}
	path, err := s.manifestPath(point.ID)
	if err != nil {
		return "", err
	}
	relative, err := s.relative(path)
	if err != nil {
		return "", err
	}
	if err := s.secureDirectory(pathpkg.Dir(relative)); err != nil {
		return "", err
	}
	err = atomicfile.WriteRoot(s.root, relative, 0o600, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(point)
	})
	return path, err
}
func (s *fileStore) readRecoveryPoint(id string) (model.RecoveryPoint, error) {
	path, err := s.manifestPath(id)
	if err != nil {
		return model.RecoveryPoint{}, err
	}
	point, err := s.readRecoveryPointFile(path, id)
	if err != nil {
		return point, err
	}
	return point, s.validateRecoveryPoint(point)
}
func (s *fileStore) readRecoveryPoints(ctx context.Context) ([]model.RecoveryPoint, error) {
	if err := s.secureDirectory("recovery-points"); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(s.root.FS(), "recovery-points")
	if err != nil {
		return nil, err
	}
	var result []model.RecoveryPoint
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || !safeID.MatchString(entry.Name()) {
			continue
		}
		point, err := s.readRecoveryPointFile(filepath.Join(s.recoveryRoot(), entry.Name(), "manifest.json"), entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			err = s.validateRecoveryPoint(point)
		}
		if err != nil {
			return nil, err
		}
		result = append(result, point)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt.Before(result[j].StartedAt) })
	return result, nil
}
func (s *fileStore) readRecoveryPointFile(filename, id string) (model.RecoveryPoint, error) {
	relative, err := s.relative(filename)
	if err != nil {
		return model.RecoveryPoint{}, err
	}
	file, err := s.root.Open(relative)
	if err != nil {
		return model.RecoveryPoint{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = errors.New("stored metadata path is not a regular file")
		}
		return model.RecoveryPoint{}, err
	}
	var point model.RecoveryPoint
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&point)
	if decodeErr == nil {
		if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
			decodeErr = errors.New("manifest must contain one JSON value")
		}
	}
	closeErr := file.Close()
	if decodeErr != nil {
		return point, fmt.Errorf("decode recovery point %s: %w", filename, decodeErr)
	}
	if closeErr != nil {
		return point, closeErr
	}
	if point.SchemaVersion != 1 || point.ID != id || !safeID.MatchString(point.ApplicationID) {
		return point, fmt.Errorf("invalid recovery point manifest %s", filename)
	}
	return point, nil
}
func (s *fileStore) validateRecoveryPoint(point model.RecoveryPoint) error {
	if point.SchemaVersion != 1 || !safeID.MatchString(point.ID) || !safeID.MatchString(point.RunID) || !safeID.MatchString(point.ApplicationID) {
		return errors.New("recovery point has invalid identifiers")
	}
	if point.Status != "running" && point.Status != "succeeded" && point.Status != "failed" && point.Status != "interrupted" {
		return errors.New("recovery point has invalid status")
	}
	if strings.ContainsAny(point.SnapshotID, "\r\n\x00/\\") || strings.HasPrefix(point.SnapshotID, "-") {
		return errors.New("recovery point has invalid snapshot ID")
	}
	staging := filepath.Join(s.dataDir, "staging", point.ID, point.ApplicationID)
	for _, dump := range point.Dumps {
		relative, err := filepath.Rel(staging, dump.Path)
		if !safeID.MatchString(dump.ID) || !filepath.IsAbs(dump.Path) || filepath.Clean(dump.Path) != dump.Path || err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || dump.Size < 0 || len(dump.SHA256) != 64 {
			return fmt.Errorf("database dump %q has invalid metadata", dump.ID)
		}
		if _, err := hex.DecodeString(dump.SHA256); err != nil {
			return fmt.Errorf("database dump %q has invalid digest", dump.ID)
		}
	}
	if verification := point.Verification; verification != nil {
		if verification.SchemaVersion != 1 || verification.RecoveryPointID != point.ID || verification.Passed < 0 || verification.Failed < 0 || verification.Unknown < 0 {
			return errors.New("recovery point has invalid verification metadata")
		}
	}
	return nil
}
func (s *fileStore) removeStaging(id string) error {
	if !safeID.MatchString(id) {
		return errors.New("invalid staging ID")
	}
	return s.root.RemoveAll(pathpkg.Join("staging", id))
}
func (s *fileStore) cleanStaging() error {
	if err := s.secureDirectory("staging"); err != nil {
		return err
	}
	entries, err := fs.ReadDir(s.root.FS(), "staging")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := s.root.RemoveAll(pathpkg.Join("staging", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func hashFile(ctx context.Context, path string) (int64, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", errors.New("dump is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			size += int64(count)
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, "", readErr
		}
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
func nowPointer() *time.Time { value := time.Now().UTC(); return &value }
