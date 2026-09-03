package appbackup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/model"
)

func TestStartupReconcilesInterruptedRecoveryPointAndStaging(t *testing.T) {
	dataDir := t.TempDir()
	cfg := serviceConfig(dataDir, t.TempDir())
	var events []string
	options := ServiceOptions{Restic: &fakeRestic{}, Databases: fakeDatabases{}, Hooks: fakeHooks{events: &events}}
	service, err := OpenService(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	point := model.RecoveryPoint{SchemaVersion: 1, ID: "unfinished", RunID: "run", ApplicationID: "site", Status: "running", StartedAt: time.Now().UTC(), Hooks: []model.HookResult{{Phase: "quiesce", Index: 0, Status: "succeeded", StartedAt: time.Now().UTC()}}}
	if _, err := service.store.writeRecoveryPoint(point); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dataDir, "staging", "stale", "file")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenService(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reconciled, err := reopened.GetRecoveryPoint(context.Background(), point.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != "interrupted" || reconciled.CompletedAt == nil {
		t.Fatalf("point = %#v", reconciled)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging remains: %v", err)
	}
	if strings.Join(events, ",") != "thaw,post" {
		t.Fatalf("cleanup hook events = %#v", events)
	}
}
