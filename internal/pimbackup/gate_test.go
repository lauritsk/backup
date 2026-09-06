package pimbackup

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func TestServiceDefersReconciliationWhileAnotherOperationRuns(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	options := ServiceOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	first, err := OpenService(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	release, err := first.gate.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}

	second, err := OpenService(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.initialized.Load() {
		t.Fatal("second service initialized while another process held the lock")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Verify(context.Background(), model.VerifyRequest{}); err != nil {
		t.Fatalf("Verify() after operation lock release = %v", err)
	}
	if !second.initialized.Load() {
		t.Fatal("second service did not initialize after lock release")
	}
}
