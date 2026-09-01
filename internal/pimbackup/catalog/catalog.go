// Package catalog stores PIM Backup operational metadata in SQLite.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	runmodel "github.com/lauritsk/backup/internal/run"
)

var ErrNotFound = errors.New("catalog record not found")

type Catalog struct {
	db   *sql.DB
	path string
}

type Run struct {
	runmodel.Record
	Detail json.RawMessage `json:"detail,omitempty"`
}

func Open(ctx context.Context, dataDir string) (*Catalog, error) {
	info, err := os.Lstat(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(dataDir, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
			return nil, fmt.Errorf("create data directory: %w", mkdirErr)
		}
		info, err = os.Lstat(dataDir)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect data directory: %w", err)
	}
	if !info.Mode().IsDir() {
		return nil, fmt.Errorf("data path %s is not a directory", dataDir)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("set data directory permissions: %w", err)
	}
	databasePath := filepath.Join(dataDir, "pim.db")
	if info, err := os.Lstat(databasePath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("catalog path %s is not a regular file", databasePath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect catalog path: %w", err)
	} else {
		file, createErr := os.OpenFile(databasePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			info, inspectErr := os.Lstat(databasePath)
			if inspectErr != nil {
				return nil, fmt.Errorf("inspect concurrently created catalog: %w", inspectErr)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("catalog path %s is not a regular file", databasePath)
			}
		} else if createErr != nil {
			return nil, fmt.Errorf("create catalog file: %w", createErr)
		} else if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("close new catalog file: %w", closeErr)
		}
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		return nil, fmt.Errorf("set catalog permissions: %w", err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	catalog := &Catalog{db: db, path: databasePath}
	if err := catalog.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return catalog, nil
}

func (c *Catalog) Close() error {
	return c.db.Close()
}

func (c *Catalog) Path() string {
	return c.path
}

func (c *Catalog) Ping(ctx context.Context) error {
	if err := c.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping catalog: %w", err)
	}
	return nil
}

func (c *Catalog) QuickCheck(ctx context.Context) error {
	var result string
	if err := c.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("run SQLite quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check: %s", result)
	}
	return nil
}
