package operationexecutor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	runmodel "github.com/lauritsk/backup/internal/run"
)

type testRun struct {
	ID        string
	Operation runmodel.Operation
	Status    runmodel.Status
	Error     error
}
type testCatalog struct{ record testRun }

func (c *testCatalog) CreateRun(_ context.Context, operation runmodel.Operation, _ any) (testRun, error) {
	c.record = testRun{ID: "one", Operation: operation, Status: runmodel.StatusQueued}
	return c.record, nil
}
func (c *testCatalog) StartRun(_ context.Context, id string) error {
	if id != c.record.ID || c.record.Status != runmodel.StatusQueued {
		return errors.New("bad start")
	}
	c.record.Status = runmodel.StatusRunning
	return nil
}
func (c *testCatalog) FinishRun(_ context.Context, id string, status runmodel.Status, runErr error, _ any) error {
	if id != c.record.ID || c.record.Status != runmodel.StatusRunning {
		return errors.New("bad finish")
	}
	c.record.Status, c.record.Error = status, runErr
	return nil
}
func (c *testCatalog) GetRun(context.Context, string) (testRun, error) { return c.record, nil }

func TestExecutorRecoversPanicAndFinishesRun(t *testing.T) {
	catalog := &testCatalog{}
	released := false
	executor := New(context.Background(), catalog, func() (func() error, error) {
		return func() error { released = true; return nil }, nil
	}, func(context.Context) error { return nil }, func(record testRun) (string, runmodel.Operation) {
		return record.ID, record.Operation
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	record, err := executor.Run(context.Background(), runmodel.OperationBackup, struct{}{}, func(context.Context, string) (any, error) {
		panic("broken")
	})
	if err == nil || record.Status != runmodel.StatusFailed || catalog.record.Error == nil || !released {
		t.Fatalf("Run() = %#v, %v, released=%v", record, err, released)
	}
}

func TestExecutorRecordsCancellation(t *testing.T) {
	catalog := &testCatalog{}
	executor := New(context.Background(), catalog, func() (func() error, error) {
		return func() error { return nil }, nil
	}, func(context.Context) error { return nil }, func(record testRun) (string, runmodel.Operation) {
		return record.ID, record.Operation
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record, err := executor.Run(ctx, runmodel.OperationVerify, struct{}{}, func(ctx context.Context, _ string) (any, error) {
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || record.Status != runmodel.StatusCanceled {
		t.Fatalf("Run() = %#v, %v", record, err)
	}
}
