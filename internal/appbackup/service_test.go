package appbackup

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/config"
	"github.com/lauritsk/backup/internal/appbackup/model"
	runmodel "github.com/lauritsk/backup/internal/run"
)

type fakeRestic struct {
	events    *[]string
	snapshots map[string]map[string][]byte
}

func (f *fakeRestic) add(value string) {
	if f.events != nil {
		*f.events = append(*f.events, value)
	}
}
func (f *fakeRestic) Version(context.Context) (string, error) {
	f.add("restic-version")
	return "restic 1.0", nil
}
func (f *fakeRestic) EnsureRepository(context.Context) error { f.add("restic-init"); return nil }
func (f *fakeRestic) CheckRepository(context.Context) error  { f.add("restic-repository"); return nil }
func (f *fakeRestic) Check(context.Context) error            { f.add("restic-check"); return nil }
func (f *fakeRestic) Backup(_ context.Context, paths, tags []string) (string, error) {
	f.add("restic-backup")
	files := map[string][]byte{}
	for _, root := range paths {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				contents, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] = contents
			}
			return nil
		})
	}
	if f.snapshots == nil {
		f.snapshots = map[string]map[string][]byte{}
	}
	f.snapshots["snapshot-1"] = files
	return "snapshot-1", nil
}
func (f *fakeRestic) Restore(_ context.Context, snapshot, target string) error {
	f.add("restic-restore")
	files, ok := f.snapshots[snapshot]
	if !ok {
		return errors.New("snapshot missing")
	}
	for source, contents := range files {
		name := filepath.Join(target, strings.TrimPrefix(filepath.Clean(source), string(filepath.Separator)))
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(name, contents, 0o600); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeRestic) List(_ context.Context, snapshot string, limit, offset int) ([]string, error) {
	f.add("restic-list")
	var result []string
	for path := range f.snapshots[snapshot] {
		result = append(result, path)
	}
	if offset >= len(result) {
		return []string{}, nil
	}
	result = result[offset:]
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

type fakeDatabases struct{ events *[]string }

func (f fakeDatabases) add(value string) {
	if f.events != nil {
		*f.events = append(*f.events, value)
	}
}
func (f fakeDatabases) Version(context.Context, config.DatabaseConfig) (string, error) {
	f.add("database-version")
	return "database 1.0", nil
}
func (f fakeDatabases) Dump(_ context.Context, _ config.DatabaseConfig, path string) error {
	f.add("database-dump")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("dump contents"), 0o600)
}
func (f fakeDatabases) VerifyDump(_ context.Context, _ config.DatabaseConfig, path string) (string, error) {
	f.add("database-verify")
	contents, err := os.ReadFile(path)
	if err != nil {
		return "failed", err
	}
	if string(contents) != "dump contents" {
		return "failed", errors.New("wrong dump")
	}
	return "passed", nil
}
func (f fakeDatabases) Check(context.Context, config.DatabaseConfig) error { return nil }

type fakeHooks struct {
	events *[]string
	fail   string
}

func (f fakeHooks) Run(_ context.Context, command config.CommandConfig) error {
	*f.events = append(*f.events, command.Binary)
	if command.Binary == f.fail {
		return errors.New("hook failed")
	}
	return nil
}

func serviceConfig(dataDir, source string) config.Config {
	minute := config.Duration{Duration: time.Minute}
	return config.Config{
		DataDir: dataDir,
		Restic:  config.ResticConfig{Binary: "restic", ResolvedPassword: "secret"},
		Applications: []config.ApplicationConfig{{
			ID: "site", Paths: []string{source}, Timeout: config.Duration{Duration: time.Hour}, VerifyAfterBackup: true,
			Databases: []config.DatabaseConfig{{ID: "db", Type: "postgresql", Timeout: minute}},
			Hooks: config.HooksConfig{
				PreBackup:  []config.CommandConfig{{Binary: "pre", Timeout: minute}},
				Quiesce:    []config.CommandConfig{{Binary: "freeze", Timeout: minute}},
				Unquiesce:  []config.CommandConfig{{Binary: "thaw", Timeout: minute}},
				PostBackup: []config.CommandConfig{{Binary: "post", Timeout: minute}},
			},
		}},
	}
}

func TestBackupVerifyBrowseAndExportRoundTrip(t *testing.T) {
	dataDir, source := t.TempDir(), filepath.Join(t.TempDir(), "files")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "content.txt"), []byte("application data"), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []string
	restic := &fakeRestic{events: &events}
	service, err := OpenService(context.Background(), serviceConfig(dataDir, source), ServiceOptions{Restic: restic, Databases: fakeDatabases{&events}, Hooks: fakeHooks{events: &events}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	run, err := service.Backup(context.Background(), model.BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runmodel.StatusSucceeded {
		t.Fatalf("backup status = %s", run.Status)
	}
	points, err := service.ListRecoveryPoints(context.Background(), "site", 100, 0)
	if err != nil || len(points) != 1 || points[0].Dumps != 1 || points[0].VerificationStatus != "passed" {
		t.Fatalf("points = %#v, err = %v", points, err)
	}
	point, err := service.GetRecoveryPoint(context.Background(), points[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if point.SnapshotID != "snapshot-1" || point.Status != "succeeded" || len(point.Dumps) != 1 || point.Verification == nil || point.Verification.Passed != 2 {
		t.Fatalf("point = %#v", point)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "recovery-points", point.ID, "verification.json")); !os.IsNotExist(err) {
		t.Fatalf("separate verification report exists: %v", err)
	}
	contents, err := service.ListRecoveryPointContents(context.Background(), point.ID, 100, 0)
	if err != nil || len(contents) < 2 {
		t.Fatalf("contents = %#v, err = %v", contents, err)
	}
	verifyRun, err := service.Verify(context.Background(), model.VerifyRequest{RecoveryPointID: point.ID})
	if err != nil || verifyRun.Status != runmodel.StatusSucceeded {
		t.Fatalf("verify = %#v, %v", verifyRun, err)
	}
	exportRun, err := service.Export(context.Background(), model.ExportRequest{RecoveryPointID: point.ID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	var report model.ExportReport
	if err := json.Unmarshal(exportRun.Detail, &report); err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(report.Directory, strings.TrimPrefix(source, string(filepath.Separator)), "content.txt")
	if contents, err := os.ReadFile(exported); err != nil || string(contents) != "application data" {
		t.Fatalf("exported = %q, %v", contents, err)
	}
	wantOrder := []string{"restic-init", "restic-version", "pre", "freeze", "database-version", "database-dump", "restic-backup", "thaw", "post", "restic-list", "restic-restore", "database-verify", "restic-list", "restic-check", "restic-list", "restic-restore", "database-verify", "restic-restore"}
	if strings.Join(events[:len(wantOrder)], ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("events = %#v", events)
	}
}

func TestOpenServiceRejectsSymlinkedDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatal(err)
	}
	cfg := serviceConfig(dataDir, t.TempDir())
	if _, err := OpenService(context.Background(), cfg, ServiceOptions{Restic: &fakeRestic{}, Databases: fakeDatabases{}, Hooks: fakeHooks{events: new([]string)}}); err == nil {
		t.Fatal("OpenService accepted a symlinked staging directory")
	}
}

func TestBackupRunsCleanupHooksAfterQuiesceFailure(t *testing.T) {
	var events []string
	cfg := serviceConfig(t.TempDir(), t.TempDir())
	restic := &fakeRestic{events: &events}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{Restic: restic, Databases: fakeDatabases{&events}, Hooks: fakeHooks{events: &events, fail: "freeze"}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	run, err := service.Backup(context.Background(), model.BackupRequest{})
	if err == nil || run.Status != runmodel.StatusFailed {
		t.Fatalf("run = %#v, err = %v", run, err)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "pre,freeze,thaw,post") || strings.Contains(joined, "database-dump") || strings.Contains(joined, "restic-backup") {
		t.Fatalf("events = %s", joined)
	}
}
