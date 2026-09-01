package pimbackup

import (
	"context"
	"errors"
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
	release, err := first.gate.tryAcquire()
	if err != nil {
		t.Fatal(err)
	}

	second, err := OpenService(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Ready(context.Background()); err == nil {
		t.Fatal("second service was ready before startup reconciliation")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Verify(context.Background(), model.VerifyRequest{}); err != nil {
		t.Fatalf("Verify() after operation lock release = %v", err)
	}
	if err := second.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() after deferred reconciliation = %v", err)
	}
}

func TestOperationGateExcludesProcessesSharingData(t *testing.T) {
	dataDir := t.TempDir()
	first, err := newOperationGate(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newOperationGate(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	releaseFirst, err := first.tryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.tryAcquire(); !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("same gate error = %v", err)
	}
	if _, err := second.tryAcquire(); !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("second gate error = %v", err)
	}
	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}
	releaseSecond, err := second.tryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseSecond(); err != nil {
		t.Fatal(err)
	}
}
