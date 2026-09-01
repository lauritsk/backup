package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"uuid"

	runmodel "github.com/lauritsk/backup/internal/run"
)

func (c *Catalog) CreateRun(ctx context.Context, operation runmodel.Operation, detail any) (Run, error) {
	if !operation.Valid() {
		return Run{}, fmt.Errorf("invalid run operation %q", operation)
	}
	id := uuid.NewV7().String()
	detailJSON, err := encodeDetail(detail)
	if err != nil {
		return Run{}, err
	}
	now := time.Now().UTC()
	_, err = c.db.ExecContext(ctx, `
INSERT INTO runs (id, operation, status, requested_at, detail_json)
VALUES (?, ?, ?, ?, ?)
`, id, operation, runmodel.StatusQueued, formatTime(now), string(detailJSON))
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}
	return c.GetRun(ctx, id)
}

func (c *Catalog) StartRun(ctx context.Context, id string) error {
	now := formatTime(time.Now().UTC())
	result, err := c.db.ExecContext(ctx, `
UPDATE runs SET status = ?, started_at = ?, finished_at = NULL, error = ''
WHERE id = ? AND status = ?
`, runmodel.StatusRunning, now, id, runmodel.StatusQueued)
	if err != nil {
		return fmt.Errorf("start run: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read start run update count: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("start run %q: queued run not found", id)
	}
	return nil
}

func (c *Catalog) FinishRun(ctx context.Context, id string, status runmodel.Status, runErr error, detail any) error {
	if !status.Terminal() {
		return fmt.Errorf("finish run with non-terminal status %q", status)
	}
	errorText := ""
	if runErr != nil {
		errorText = runErr.Error()
	}
	detailJSON, err := encodeDetail(detail)
	if err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, `
UPDATE runs SET status = ?, finished_at = ?, error = ?, detail_json = ?
WHERE id = ? AND status IN (?, ?)
`, status, formatTime(time.Now().UTC()), errorText, string(detailJSON), id,
		runmodel.StatusQueued, runmodel.StatusRunning)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read finish run update count: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("finish run %q: active run not found", id)
	}
	return nil
}

func (c *Catalog) MarkInterrupted(ctx context.Context) (int64, error) {
	now := formatTime(time.Now().UTC())
	result, err := c.db.ExecContext(ctx, `
UPDATE runs SET status = ?, finished_at = ?, error = 'process stopped before the operation completed'
WHERE status IN (?, ?)
`, runmodel.StatusInterrupted, now, runmodel.StatusQueued, runmodel.StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted runs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count interrupted runs: %w", err)
	}
	return count, nil
}

func (c *Catalog) GetRun(ctx context.Context, id string) (Run, error) {
	row := c.db.QueryRowContext(ctx, runSelect+" WHERE id = ?", id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", err)
	}
	return run, nil
}

func (c *Catalog) ListRuns(ctx context.Context, limit, offset int) ([]Run, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := c.db.QueryContext(ctx, runSelect+" ORDER BY requested_at DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	runs := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}

const runSelect = `
SELECT id, operation, status, requested_at, started_at, finished_at, error, detail_json
FROM runs
`

func scanRun(row rowScanner) (Run, error) {
	var result Run
	var requestedAt, detail string
	var startedAt, finishedAt sql.NullString
	if err := row.Scan(&result.ID, &result.Operation, &result.Status, &requestedAt,
		&startedAt, &finishedAt, &result.Error, &detail); err != nil {
		return Run{}, err
	}
	var err error
	result.RequestedAt, err = parseTime(requestedAt)
	if err != nil {
		return Run{}, err
	}
	result.StartedAt, err = parseNullableTime(startedAt)
	if err != nil {
		return Run{}, err
	}
	result.FinishedAt, err = parseNullableTime(finishedAt)
	if err != nil {
		return Run{}, err
	}
	if !json.Valid([]byte(detail)) {
		return Run{}, errors.New("run detail is not valid JSON")
	}
	result.Detail = json.RawMessage(detail)
	return result, nil
}

func encodeDetail(detail any) (json.RawMessage, error) {
	if detail == nil {
		return json.RawMessage(`{}`), nil
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("encode run detail: %w", err)
	}
	return encoded, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func formatTimePointer(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return formatTime(*value)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse catalog time %q: %w", value, err)
	}
	return parsed, nil
}

func parseNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
