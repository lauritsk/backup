// Package atomicfile writes durable files on a normal local filesystem.
package atomicfile

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes a replacement file in the destination directory, syncs it,
// renames it over the destination, and syncs the parent directory.
func Write(filename string, perm fs.FileMode, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	temp, err := os.CreateTemp(dir, "."+filepath.Base(filename)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		if err != nil {
			_ = os.Remove(tempName)
		}
	}()

	if err = temp.Chmod(perm); err != nil {
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if err = write(temp); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err = os.Rename(tempName, filename); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	if err = SyncDir(dir); err != nil {
		return err
	}
	return nil
}

// SyncDir syncs a directory entry after a create, rename, or removal.
func SyncDir(dirname string) error {
	dir, err := os.Open(dirname)
	if err != nil {
		return fmt.Errorf("open parent directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}
