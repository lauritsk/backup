package cloudbackup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

type fakeRclone struct {
	files map[string]string
	fail  map[string]bool
}

func (fakeRclone) Version(context.Context) error                          { return nil }
func (fakeRclone) CheckSource(context.Context, config.SourceConfig) error { return nil }
func (f fakeRclone) Inventory(context.Context, config.SourceConfig) ([]model.RemoteFile, error) {
	files := make([]model.RemoteFile, 0, len(f.files))
	for name, contents := range f.files {
		files = append(files, model.RemoteFile{Path: name, Size: int64(len(contents)), ModTime: time.Unix(1, 0).UTC()})
	}
	return files, nil
}
func (f fakeRclone) Download(_ context.Context, _ config.SourceConfig, name string, destination io.Writer) error {
	if f.fail[name] {
		return errors.New("remote read failed")
	}
	_, err := io.WriteString(destination, f.files[name])
	return err
}

func TestBackupVerifyRestoreRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	remote := fakeRclone{files: map[string]string{"docs/report.txt": "original"}}
	cfg := config.Config{DataDir: dataDir, Rclone: config.RcloneConfig{}, Sources: []config.SourceConfig{{ID: "documents", Remote: "test:docs", Timeout: config.Duration{Duration: time.Minute}}}}
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

func TestPartialAcquisitionSurvivesOlderManifestReconciliation(t *testing.T) {
	dataDir := t.TempDir()
	remote := fakeRclone{files: map[string]string{"a.txt": "old-a", "b.txt": "old-b"}, fail: map[string]bool{}}
	cfg := config.Config{DataDir: dataDir, Rclone: config.RcloneConfig{}, Sources: []config.SourceConfig{{ID: "source", Remote: "test:", Timeout: config.Duration{Duration: time.Minute}}}}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{Rclone: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Backup(context.Background(), model.BackupRequest{}); err != nil {
		t.Fatal(err)
	}
	remote.files["a.txt"] = "new-a"
	remote.files["b.txt"] = "new-b"
	remote.fail["b.txt"] = true
	if _, err := service.Backup(context.Background(), model.BackupRequest{}); err == nil {
		t.Fatal("partial backup unexpectedly succeeded")
	}
	newRecord, err := service.GetFile(context.Background(), "source", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenService(context.Background(), cfg, ServiceOptions{Rclone: remote})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reconciled, err := reopened.GetFile(context.Background(), "source", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.SHA256 != newRecord.SHA256 || reconciled.LastRunID != newRecord.LastRunID {
		t.Fatalf("older manifest replaced partial durable commit: before=%#v after=%#v", newRecord, reconciled)
	}
}

func TestBackupDoesNotDeleteLocalFileMissingFromRemote(t *testing.T) {
	dataDir := t.TempDir()
	remote := fakeRclone{files: map[string]string{"keep.txt": "keep", "gone.txt": "retained"}}
	cfg := config.Config{DataDir: dataDir, Rclone: config.RcloneConfig{}, Sources: []config.SourceConfig{{ID: "source", Remote: "test:", Timeout: config.Duration{Duration: time.Minute}}}}
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
