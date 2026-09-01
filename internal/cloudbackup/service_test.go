package cloudbackup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

type fakeRclone struct{ files map[string]string }

func (fakeRclone) Version(context.Context) error                          { return nil }
func (fakeRclone) CheckSource(context.Context, config.SourceConfig) error { return nil }
func (f fakeRclone) Copy(_ context.Context, _ config.SourceConfig, destination string) error {
	for name, contents := range f.files {
		path := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func TestBackupVerifyRestoreRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	remote := fakeRclone{files: map[string]string{"docs/report.txt": "original"}}
	cfg := config.Config{DataDir: dataDir, Rclone: config.RcloneConfig{Binary: "rclone"}, Sources: []config.SourceConfig{{ID: "documents", Remote: "test:docs", Timeout: config.Duration{Duration: time.Minute}}}}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{Rclone: remote})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	backupRun, err := service.Backup(context.Background(), model.BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backupRun.Status != "succeeded" {
		t.Fatalf("backup status = %q", backupRun.Status)
	}
	file, err := service.GetFile(context.Background(), "documents", "docs/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := service.ListManifests(context.Background(), "documents", 100, 0)
	if err != nil || len(manifests) != 1 || manifests[0].RunID != backupRun.ID {
		t.Fatalf("manifests = %#v, err = %v", manifests, err)
	}
	if file.Size != int64(len("original")) || file.SHA256 == "" {
		t.Fatalf("file = %#v", file)
	}

	verifyRun, err := service.Verify(context.Background(), model.VerifyRequest{SourceID: "documents", Path: "docs/report.txt"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verifyRun.Status != "succeeded" {
		t.Fatalf("verify status = %q", verifyRun.Status)
	}

	restoreRun, err := service.Restore(context.Background(), model.RestoreRequest{SourceID: "documents", Paths: []string{"docs/report.txt"}, Confirm: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored := filepath.Join(dataDir, "restores", restoreRun.ID, "documents", "docs", "report.txt")
	contents, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original" {
		t.Fatalf("restored contents = %q", contents)
	}
}

func TestBackupDoesNotDeleteLocalFileMissingFromRemote(t *testing.T) {
	dataDir := t.TempDir()
	remote := fakeRclone{files: map[string]string{"keep.txt": "keep", "gone.txt": "retained"}}
	cfg := config.Config{DataDir: dataDir, Rclone: config.RcloneConfig{Binary: "rclone"}, Sources: []config.SourceConfig{{ID: "source", Remote: "test:", Timeout: config.Duration{Duration: time.Minute}}}}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{Rclone: remote})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.Backup(context.Background(), model.BackupRequest{}); err != nil {
		t.Fatal(err)
	}
	delete(remote.files, "gone.txt")
	if _, err := service.Backup(context.Background(), model.BackupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetFile(context.Background(), "source", "gone.txt"); err != nil {
		t.Fatalf("deleted remote file was not retained: %v", err)
	}
}
