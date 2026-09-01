package operationlock

import (
	"errors"
	"testing"
)

func TestGateExcludesGoroutinesAndProcesses(t *testing.T) {
	dataDir := t.TempDir()
	busy := errors.New("busy")
	first, err := New(dataDir, ".test.lock", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := New(dataDir, ".test.lock", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	release, err := first.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.TryAcquire(); !errors.Is(err, busy) {
		t.Fatalf("same gate error = %v", err)
	}
	if _, err := second.TryAcquire(); !errors.Is(err, busy) {
		t.Fatalf("second gate error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	secondRelease, err := second.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRelease(); err != nil {
		t.Fatal(err)
	}
}
