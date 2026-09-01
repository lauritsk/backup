package cloudbackup

import (
	"context"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func TestStartupReappliesOnlyLatestManifestAndKeepsVerification(t *testing.T) {
	dataDir := t.TempDir()
	remote := fakeRclone{files: map[string]string{"file.txt": "contents"}}
	cfg := config.Config{DataDir: dataDir, Rclone: config.RcloneConfig{Binary: "rclone"}, Sources: []config.SourceConfig{{ID: "source", Remote: "test:", Timeout: config.Duration{Duration: time.Minute}}}}

	service, err := OpenService(context.Background(), cfg, ServiceOptions{Rclone: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Backup(context.Background(), model.BackupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Verify(context.Background(), model.VerifyRequest{}); err != nil {
		t.Fatal(err)
	}
	verified, err := service.GetFile(context.Background(), "source", "file.txt")
	if err != nil || verified.VerifiedAt == nil {
		t.Fatalf("verified file = %#v, err = %v", verified, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenService(context.Background(), cfg, ServiceOptions{Rclone: remote})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	afterStartup, err := reopened.GetFile(context.Background(), "source", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if afterStartup.VerifiedAt == nil {
		t.Fatal("startup reconciliation cleared verification")
	}
	manifests, err := reopened.ListManifests(context.Background(), "source", 100, 0)
	if err != nil || len(manifests) != 1 {
		t.Fatalf("manifests = %#v, err = %v", manifests, err)
	}
}
