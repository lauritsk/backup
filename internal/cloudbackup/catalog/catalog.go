// Package catalog stores Cloud Backup metadata in SQLite.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/lauritsk/backup/internal/cloudbackup/model"
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
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	info, err := os.Lstat(dataDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("data path %s is not a directory", dataDir)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("set data directory permissions: %w", err)
	}
	path := filepath.Join(dataDir, "cloud.db")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("catalog path %s is not a regular file", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil && !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("create catalog: %w", createErr)
		}
		if createErr == nil {
			if closeErr := file.Close(); closeErr != nil {
				return nil, fmt.Errorf("close new catalog: %w", closeErr)
			}
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("set catalog permissions: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	catalog := &Catalog{db: db, path: path}
	if err := catalog.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return catalog, nil
}

func (c *Catalog) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`, `PRAGMA synchronous = FULL`, `PRAGMA foreign_keys = ON`, `PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, operation TEXT NOT NULL, status TEXT NOT NULL, requested_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, error TEXT NOT NULL DEFAULT '', detail_json TEXT NOT NULL DEFAULT '{}')`,
		`CREATE TABLE IF NOT EXISTS sources (id TEXT PRIMARY KEY, remote TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS files (source_id TEXT NOT NULL, path TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, mod_time TEXT NOT NULL, last_run_id TEXT NOT NULL, verified_at TEXT, verification_error TEXT NOT NULL DEFAULT '', PRIMARY KEY(source_id,path), FOREIGN KEY(source_id) REFERENCES sources(id))`,
		`CREATE INDEX IF NOT EXISTS files_source_path ON files(source_id,path)`,
	}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize catalog: %w", err)
		}
	}
	return nil
}
func (c *Catalog) Close() error                   { return c.db.Close() }
func (c *Catalog) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }
func (c *Catalog) QuickCheck(ctx context.Context) error {
	var value string
	if err := c.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&value); err != nil {
		return err
	}
	if value != "ok" {
		return fmt.Errorf("SQLite quick_check: %s", value)
	}
	return nil
}

func (c *Catalog) UpsertSource(ctx context.Context, id, remote string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO sources(id,remote,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET remote=excluded.remote,updated_at=excluded.updated_at`, id, remote, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store source: %w", err)
	}
	return nil
}

func (c *Catalog) ApplyManifest(ctx context.Context, manifest model.Manifest) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sources(id,remote,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET remote=excluded.remote,updated_at=excluded.updated_at`, manifest.SourceID, manifest.Remote, formatTime(manifest.CompletedAt)); err != nil {
		return fmt.Errorf("store manifest source: %w", err)
	}
	for _, file := range manifest.Files {
		_, err := tx.ExecContext(ctx, `
INSERT INTO files(source_id,path,size,sha256,mod_time,last_run_id,verified_at,verification_error)
VALUES(?,?,?,?,?,?,NULL,'')
ON CONFLICT(source_id,path) DO UPDATE SET
  verified_at=CASE WHEN files.size=excluded.size AND files.sha256=excluded.sha256 THEN files.verified_at ELSE NULL END,
  verification_error=CASE WHEN files.size=excluded.size AND files.sha256=excluded.sha256 THEN files.verification_error ELSE '' END,
  size=excluded.size,
  sha256=excluded.sha256,
  mod_time=excluded.mod_time,
  last_run_id=excluded.last_run_id
WHERE files.size<>excluded.size OR files.sha256<>excluded.sha256 OR files.mod_time<>excluded.mod_time OR files.last_run_id<>excluded.last_run_id
`, manifest.SourceID, file.Path, file.Size, file.SHA256, formatTime(file.ModTime), manifest.RunID)
		if err != nil {
			return fmt.Errorf("store manifest file %q: %w", file.Path, err)
		}
	}
	return tx.Commit()
}

func (c *Catalog) GetFile(ctx context.Context, sourceID, path string) (model.File, error) {
	row := c.db.QueryRowContext(ctx, fileSelect+` WHERE source_id=? AND path=?`, sourceID, path)
	return scanFile(row)
}
func (c *Catalog) ListFiles(ctx context.Context, sourceID, prefix string, limit, offset int) ([]model.File, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	query := fileSelect + ` WHERE (?='' OR source_id=?) AND (?='' OR path LIKE ? ESCAPE '\') ORDER BY source_id,path LIMIT ? OFFSET ?`
	like := escapeLike(prefix) + "%"
	rows, err := c.db.QueryContext(ctx, query, sourceID, sourceID, prefix, like, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var result []model.File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, file)
	}
	return result, rows.Err()
}
func (c *Catalog) AllFiles(ctx context.Context, sourceID string) ([]model.File, error) {
	return c.ListFiles(ctx, sourceID, "", 1000, 0)
}
func (c *Catalog) ForEachFile(ctx context.Context, sourceID string, action func(model.File) error) error {
	rows, err := c.db.QueryContext(ctx, fileSelect+` WHERE (?='' OR source_id=?) ORDER BY source_id,path`, sourceID, sourceID)
	if err != nil {
		return err
	}
	var files []model.File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, file := range files {
		if err := action(file); err != nil {
			return err
		}
	}
	return nil
}

const fileSelect = `SELECT source_id,path,size,sha256,mod_time,last_run_id,verified_at,verification_error FROM files`

type scanner interface{ Scan(...any) error }

func scanFile(row scanner) (model.File, error) {
	var result model.File
	var mod string
	var verified sql.NullString
	if err := row.Scan(&result.SourceID, &result.Path, &result.Size, &result.SHA256, &mod, &result.LastRunID, &verified, &result.VerificationError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrNotFound
		}
		return result, err
	}
	var err error
	result.ModTime, err = parseTime(mod)
	if err != nil {
		return result, err
	}
	if verified.Valid {
		value, err := parseTime(verified.String)
		if err != nil {
			return result, err
		}
		result.VerifiedAt = &value
	}
	return result, nil
}
func (c *Catalog) UpdateVerification(ctx context.Context, sourceID, path string, verifyErr error) error {
	message := ""
	if verifyErr != nil {
		message = verifyErr.Error()
	}
	_, err := c.db.ExecContext(ctx, `UPDATE files SET verified_at=?,verification_error=? WHERE source_id=? AND path=?`, formatTime(time.Now()), message, sourceID, path)
	return err
}

func (c *Catalog) ListSources(ctx context.Context) ([]model.Source, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT s.id,s.remote,COUNT(f.path),COALESCE(SUM(f.size),0) FROM sources s LEFT JOIN files f ON f.source_id=s.id GROUP BY s.id,s.remote ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Source
	for rows.Next() {
		var source model.Source
		if err := rows.Scan(&source.ID, &source.Remote, &source.Files, &source.Bytes); err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

func (c *Catalog) CreateRun(ctx context.Context, operation runmodel.Operation, request any) (Run, error) {
	if !operation.Valid() {
		return Run{}, fmt.Errorf("invalid operation %q", operation)
	}
	detail, err := json.Marshal(request)
	if err != nil {
		return Run{}, err
	}
	id := uuid.NewString()
	now := formatTime(time.Now())
	_, err = c.db.ExecContext(ctx, `INSERT INTO runs(id,operation,status,requested_at,detail_json) VALUES(?,?,?,?,?)`, id, operation, runmodel.StatusQueued, now, string(detail))
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}
	return c.GetRun(ctx, id)
}
func (c *Catalog) StartRun(ctx context.Context, id string) error {
	result, err := c.db.ExecContext(ctx, `UPDATE runs SET status=?,started_at=?,finished_at=NULL,error='' WHERE id=? AND status=?`, runmodel.StatusRunning, formatTime(time.Now()), id, runmodel.StatusQueued)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("queued run not found")
	}
	return nil
}
func (c *Catalog) FinishRun(ctx context.Context, id string, status runmodel.Status, runErr error, detail any) error {
	if !status.Terminal() {
		return errors.New("run status is not terminal")
	}
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, `UPDATE runs SET status=?,finished_at=?,error=?,detail_json=? WHERE id=? AND status IN (?,?)`, status, formatTime(time.Now()), message, string(encoded), id, runmodel.StatusQueued, runmodel.StatusRunning)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("active run not found")
	}
	return nil
}
func (c *Catalog) MarkInterrupted(ctx context.Context) (int64, error) {
	result, err := c.db.ExecContext(ctx, `UPDATE runs SET status=?,finished_at=?,error='process stopped before the operation completed' WHERE status IN (?,?)`, runmodel.StatusInterrupted, formatTime(time.Now()), runmodel.StatusQueued, runmodel.StatusRunning)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
func (c *Catalog) GetRun(ctx context.Context, id string) (Run, error) {
	return scanRun(c.db.QueryRowContext(ctx, runSelect+` WHERE id=?`, id))
}
func (c *Catalog) ListRuns(ctx context.Context, limit, offset int) ([]Run, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := c.db.QueryContext(ctx, runSelect+` ORDER BY requested_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

const runSelect = `SELECT id,operation,status,requested_at,started_at,finished_at,error,detail_json FROM runs`

func scanRun(row scanner) (Run, error) {
	var result Run
	var requested, detail string
	var started, finished sql.NullString
	if err := row.Scan(&result.ID, &result.Operation, &result.Status, &requested, &started, &finished, &result.Error, &detail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrNotFound
		}
		return result, err
	}
	var err error
	result.RequestedAt, err = parseTime(requested)
	if err != nil {
		return result, err
	}
	if started.Valid {
		value, e := parseTime(started.String)
		if e != nil {
			return result, e
		}
		result.StartedAt = &value
	}
	if finished.Valid {
		value, e := parseTime(finished.String)
		if e != nil {
			return result, e
		}
		result.FinishedAt = &value
	}
	if !json.Valid([]byte(detail)) {
		return result, errors.New("invalid run detail")
	}
	result.Detail = json.RawMessage(detail)
	return result, nil
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}
func formatTime(value time.Time) string         { return value.UTC().Format(time.RFC3339Nano) }
func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
