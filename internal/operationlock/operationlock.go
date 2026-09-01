// Package operationlock excludes operations across goroutines and processes.
package operationlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

// Gate combines a process-local guard with an advisory filesystem lock.
type Gate struct {
	mutex   sync.Mutex
	active  bool
	file    *flock.Flock
	busyErr error
}

// New creates a gate using a regular lock file beneath dataDir.
func New(dataDir, filename string, busyErr error) (*Gate, error) {
	if busyErr == nil {
		return nil, errors.New("operation lock requires a busy error")
	}
	lockPath := filepath.Join(dataDir, filename)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	switch {
	case err == nil:
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close new operation lock file: %w", err)
		}
	case errors.Is(err, os.ErrExist):
		info, inspectErr := os.Lstat(lockPath)
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect operation lock file: %w", inspectErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("operation lock path %s is not a regular file", lockPath)
		}
	default:
		return nil, fmt.Errorf("create operation lock file: %w", err)
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		return nil, fmt.Errorf("set operation lock permissions: %w", err)
	}
	return &Gate{file: flock.New(lockPath), busyErr: busyErr}, nil
}

// TryAcquire returns immediately when this or another process owns the lock.
func (g *Gate) TryAcquire() (func() error, error) {
	g.mutex.Lock()
	if g.active {
		g.mutex.Unlock()
		return nil, g.busyErr
	}
	g.active = true
	g.mutex.Unlock()

	locked, err := g.file.TryLock()
	if err != nil {
		g.clear()
		return nil, fmt.Errorf("acquire operation lock: %w", err)
	}
	if !locked {
		g.clear()
		return nil, g.busyErr
	}

	var once sync.Once
	return func() error {
		var unlockErr error
		once.Do(func() {
			unlockErr = g.file.Unlock()
			g.clear()
		})
		return unlockErr
	}, nil
}

func (g *Gate) Close() error { return g.file.Close() }

func (g *Gate) clear() {
	g.mutex.Lock()
	g.active = false
	g.mutex.Unlock()
}
