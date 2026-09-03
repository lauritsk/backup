package restic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerUsesLocalRepositoryAndPasswordEnvironment(t *testing.T) {
	dataDir := t.TempDir()
	binary := filepath.Join(t.TempDir(), "restic")
	capture := filepath.Join(t.TempDir(), "capture")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$RESTIC_REPOSITORY|$RESTIC_PASSWORD|$*\" >> '" + capture + "'\n" +
		"case \"$1\" in\n" +
		"version) echo 'restic 1.0.0';;\n" +
		"snapshots) exit 1;;\n" +
		"init) exit 0;;\n" +
		"backup) echo '{\"message_type\":\"summary\",\"snapshot_id\":\"abc123\"}';;\n" +
		"check) exit 0;;\n" +
		"ls) echo '{\"struct_type\":\"node\",\"type\":\"file\",\"path\":\"/srv/file\"}';;\n" +
		"*) exit 1;; esac\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := Runner{Binary: binary, Repository: filepath.Join(dataDir, "restic"), Password: "secret", DataDir: dataDir}
	if err := runner.EnsureRepository(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runner.Backup(context.Background(), []string{"/srv/data"}, []string{"appbackup"})
	if err != nil || snapshot != "abc123" {
		t.Fatalf("Backup = %q, %v", snapshot, err)
	}
	paths, err := runner.List(context.Background(), snapshot, 100, 0)
	if err != nil || len(paths) != 1 {
		t.Fatalf("List = %#v, %v", paths, err)
	}
	contents, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), filepath.Join(dataDir, "restic")+"|secret|") {
		t.Fatalf("capture = %q", contents)
	}
	if err := runner.Restore(context.Background(), snapshot, filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("Restore accepted a destination outside data_dir")
	}
	if _, err := runner.Backup(context.Background(), []string{dataDir}, nil); err == nil {
		t.Fatal("Backup accepted a path containing its repository")
	}
}
