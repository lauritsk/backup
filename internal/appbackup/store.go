package appbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/atomicfile"
)

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type fileStore struct{ dataDir string }

func newFileStore(dataDir string) (*fileStore, error) {
	store := &fileStore{dataDir: dataDir}
	for _, path := range []string{store.recoveryRoot(), filepath.Join(dataDir, "staging"), filepath.Join(dataDir, "restores"), filepath.Join(dataDir, "restic")} {
		if err := secureDirectory(dataDir, path); err != nil {
			return nil, err
		}
	}
	return store, nil
}
func secureDirectory(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("directory escapes the data root")
	}
	current := root
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a plain directory", current)
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return err
		}
	}
	return nil
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
	path := filepath.Join(s.dataDir, "staging", id, app)
	return path, secureDirectory(s.dataDir, path)
}
func (s *fileStore) prepareDumpDir(staging string) error {
	return secureDirectory(s.dataDir, filepath.Join(staging, "dumps"))
}
func (s *fileStore) restoreDir(runID, pointID string) (string, error) {
	if !safeID.MatchString(runID) || !safeID.MatchString(pointID) {
		return "", errors.New("invalid recovery point ID")
	}
	path := filepath.Join(s.dataDir, "restores", runID, pointID)
	return path, secureDirectory(s.dataDir, path)
}

func (s *fileStore) writeRecoveryPoint(point model.RecoveryPoint) (string, error) {
	if err := s.validateRecoveryPoint(point); err != nil {
		return "", err
	}
	path, err := s.manifestPath(point.ID)
	if err != nil {
		return "", err
	}
	if err := secureDirectory(s.dataDir, filepath.Dir(path)); err != nil {
		return "", err
	}
	err = atomicfile.Write(path, 0o600, func(writer io.Writer) error {
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
	point, err := readRecoveryPointFile(path, id)
	if err != nil {
		return point, err
	}
	return point, s.validateRecoveryPoint(point)
}
func (s *fileStore) readRecoveryPoints(ctx context.Context) ([]model.RecoveryPoint, error) {
	if err := secureDirectory(s.dataDir, s.recoveryRoot()); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.recoveryRoot())
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
		point, err := readRecoveryPointFile(filepath.Join(s.recoveryRoot(), entry.Name(), "manifest.json"), entry.Name())
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
func readRecoveryPointFile(path, id string) (model.RecoveryPoint, error) {
	file, err := openRegular(path)
	if err != nil {
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
		return point, fmt.Errorf("decode recovery point %s: %w", path, decodeErr)
	}
	if closeErr != nil {
		return point, closeErr
	}
	if point.SchemaVersion != 1 || point.ID != id || !safeID.MatchString(point.ApplicationID) {
		return point, fmt.Errorf("invalid recovery point manifest %s", path)
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
func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("stored metadata path is not a regular file")
	}
	return os.Open(path)
}

func (s *fileStore) removeStaging(id string) error {
	if !safeID.MatchString(id) {
		return errors.New("invalid staging ID")
	}
	return os.RemoveAll(filepath.Join(s.dataDir, "staging", id))
}
func (s *fileStore) cleanStaging() error {
	root := filepath.Join(s.dataDir, "staging")
	if err := secureDirectory(s.dataDir, root); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
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
