// Package catalog stores Application Backup operational metadata in SQLite.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
	"uuid"

	"github.com/lauritsk/backup/internal/appbackup/model"
	runmodel "github.com/lauritsk/backup/internal/run"
)

var ErrNotFound = errors.New("catalog record not found")

type Catalog struct {
	db      *sql.DB
	created bool
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
		return nil, err
	}
	path := filepath.Join(dataDir, "app.db")
	created := false
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("catalog path %s is not a regular file", path)
	} else if err == nil {
		created = info.Size() == 0
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil && !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("create catalog: %w", createErr)
		}
		if createErr == nil {
			created = true
			if closeErr := file.Close(); closeErr != nil {
				return nil, closeErr
			}
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	catalog := &Catalog{db: db, created: created}
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
		`CREATE TABLE IF NOT EXISTS applications (id TEXT PRIMARY KEY, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS recovery_points (id TEXT PRIMARY KEY, application_id TEXT NOT NULL, run_id TEXT NOT NULL, status TEXT NOT NULL, started_at TEXT NOT NULL, completed_at TEXT, snapshot_id TEXT NOT NULL DEFAULT '', manifest_path TEXT NOT NULL, dump_count INTEGER NOT NULL DEFAULT 0, verified_at TEXT, verification_status TEXT NOT NULL DEFAULT '', FOREIGN KEY(application_id) REFERENCES applications(id))`,
		`CREATE INDEX IF NOT EXISTS recovery_points_application_time ON recovery_points(application_id,started_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize catalog: %w", err)
		}
	}
	return nil
}

func (c *Catalog) Close() error                   { return c.db.Close() }
func (c *Catalog) Created() bool                  { return c.created }
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
func (c *Catalog) UpsertApplication(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO applications(id,updated_at) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at`, id, formatTime(time.Now()))
	return err
}

func (c *Catalog) PrepareRebuild(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_points`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM applications`); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) ApplyRecoveryPoint(ctx context.Context, point model.RecoveryPoint, manifestPath string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO applications(id,updated_at) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at`, point.ApplicationID, formatTime(point.StartedAt)); err != nil {
		return err
	}
	completed := any(nil)
	if point.CompletedAt != nil {
		completed = formatTime(*point.CompletedAt)
	}
	verified, verificationStatus := any(nil), ""
	if point.Verification != nil {
		verified = formatTime(point.Verification.VerifiedAt)
		verificationStatus = verificationStatusFor(*point.Verification)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recovery_points(id,application_id,run_id,status,started_at,completed_at,snapshot_id,manifest_path,dump_count,verified_at,verification_status) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET application_id=excluded.application_id,run_id=excluded.run_id,status=excluded.status,started_at=excluded.started_at,completed_at=excluded.completed_at,snapshot_id=excluded.snapshot_id,manifest_path=excluded.manifest_path,dump_count=excluded.dump_count,verified_at=excluded.verified_at,verification_status=excluded.verification_status`, point.ID, point.ApplicationID, point.RunID, point.Status, formatTime(point.StartedAt), completed, point.SnapshotID, manifestPath, len(point.Dumps), verified, verificationStatus)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) ListApplications(ctx context.Context) ([]model.Application, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT a.id,COUNT(r.id),MAX(CASE WHEN r.status='succeeded' THEN r.completed_at END) FROM applications a LEFT JOIN recovery_points r ON r.application_id=a.id GROUP BY a.id ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Application
	for rows.Next() {
		var app model.Application
		var last sql.NullString
		if err := rows.Scan(&app.ID, &app.RecoveryPoints, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			value, err := parseTime(last.String)
			if err != nil {
				return nil, err
			}
			app.LastBackupAt = &value
		}
		result = append(result, app)
	}
	return result, rows.Err()
}

func (c *Catalog) ListRecoveryPoints(ctx context.Context, application string, limit, offset int) ([]model.RecoveryPointSummary, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := c.db.QueryContext(ctx, `SELECT id,application_id,status,started_at,completed_at,snapshot_id,verification_status,dump_count FROM recovery_points WHERE (?='' OR application_id=?) ORDER BY started_at DESC LIMIT ? OFFSET ?`, application, application, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.RecoveryPointSummary
	for rows.Next() {
		point, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, point)
	}
	return result, rows.Err()
}
func (c *Catalog) RecoveryPointsForVerification(ctx context.Context, id, application string, all bool) ([]model.RecoveryPointSummary, error) {
	if id != "" {
		point, err := c.GetRecoveryPoint(ctx, id)
		if err != nil {
			return nil, err
		}
		return []model.RecoveryPointSummary{point}, nil
	}
	query := `SELECT id,application_id,status,started_at,completed_at,snapshot_id,verification_status,dump_count FROM recovery_points`
	if all {
		query += ` WHERE snapshot_id<>'' AND (?='' OR application_id=?)`
	} else {
		query += ` AS current WHERE current.status='succeeded' AND current.snapshot_id<>'' AND (?='' OR current.application_id=?) AND current.id = (
			SELECT candidate.id FROM recovery_points AS candidate
			WHERE candidate.application_id=current.application_id AND candidate.status='succeeded' AND candidate.snapshot_id<>''
			ORDER BY candidate.started_at DESC LIMIT 1
		)`
	}
	query += ` ORDER BY started_at DESC`
	rows, err := c.db.QueryContext(ctx, query, application, application)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.RecoveryPointSummary
	for rows.Next() {
		point, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, point)
	}
	return result, rows.Err()
}
func (c *Catalog) GetRecoveryPoint(ctx context.Context, id string) (model.RecoveryPointSummary, error) {
	return scanSummary(c.db.QueryRowContext(ctx, `SELECT id,application_id,status,started_at,completed_at,snapshot_id,verification_status,dump_count FROM recovery_points WHERE id=?`, id))
}

type scanner interface{ Scan(...any) error }

func scanSummary(row scanner) (model.RecoveryPointSummary, error) {
	var point model.RecoveryPointSummary
	var started string
	var completed sql.NullString
	if err := row.Scan(&point.ID, &point.ApplicationID, &point.Status, &started, &completed, &point.SnapshotID, &point.VerificationStatus, &point.Dumps); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return point, ErrNotFound
		}
		return point, err
	}
	var err error
	point.StartedAt, err = parseTime(started)
	if err != nil {
		return point, err
	}
	if completed.Valid {
		value, err := parseTime(completed.String)
		if err != nil {
			return point, err
		}
		point.CompletedAt = &value
	}
	return point, nil
}
func verificationStatusFor(record model.VerificationRecord) string {
	if record.Failed > 0 {
		return "failed"
	}
	if record.Unknown > 0 {
		return "unknown"
	}
	return "passed"
}

func (c *Catalog) CreateRun(ctx context.Context, operation runmodel.Operation, request any) (Run, error) {
	if !operation.Valid() {
		return Run{}, fmt.Errorf("invalid operation %q", operation)
	}
	detail, err := json.Marshal(request)
	if err != nil {
		return Run{}, err
	}
	id, now := uuid.NewV7().String(), formatTime(time.Now())
	_, err = c.db.ExecContext(ctx, `INSERT INTO runs(id,operation,status,requested_at,detail_json) VALUES(?,?,?,?,?)`, id, operation, runmodel.StatusQueued, now, string(detail))
	if err != nil {
		return Run{}, err
	}
	return c.GetRun(ctx, id)
}
func (c *Catalog) StartRun(ctx context.Context, id string) error {
	result, err := c.db.ExecContext(ctx, `UPDATE runs SET status=?,started_at=?,finished_at=NULL,error='' WHERE id=? AND status=?`, runmodel.StatusRunning, formatTime(time.Now()), id, runmodel.StatusQueued)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
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
	count, _ := result.RowsAffected()
	if count != 1 {
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
		value, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
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
func formatTime(value time.Time) string         { return value.UTC().Format(time.RFC3339Nano) }
func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
