package pimbackup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

var ErrOperationBusy = errors.New("another backup, verification, restore, or rebuild is already running")

type operationGate struct {
	mutex  sync.Mutex
	active bool
	file   *flock.Flock
}

func newOperationGate(dataDir string) (*operationGate, error) {
	lockPath := filepath.Join(dataDir, ".pimbackup.lock")
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
	return &operationGate{file: flock.New(lockPath)}, nil
}

func (g *operationGate) tryAcquire() (func() error, error) {
	g.mutex.Lock()
	if g.active {
		g.mutex.Unlock()
		return nil, ErrOperationBusy
	}
	g.active = true
	g.mutex.Unlock()

	locked, err := g.file.TryLock()
	if err != nil {
		g.clearActive()
		return nil, fmt.Errorf("acquire operation lock: %w", err)
	}
	if !locked {
		g.clearActive()
		return nil, ErrOperationBusy
	}

	var once sync.Once
	return func() error {
		var unlockErr error
		once.Do(func() {
			unlockErr = g.file.Unlock()
			g.clearActive()
		})
		return unlockErr
	}, nil
}

func (g *operationGate) clearActive() {
	g.mutex.Lock()
	g.active = false
	g.mutex.Unlock()
}
