package pimbackup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/buildinfo"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"version"}, &stdout, &stderr, buildinfo.Info{Version: "test"})

	if code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "pimbackup version=test ") {
		t.Fatalf("Run() output = %q", stdout.String())
	}
}

func TestConfigShowRedactsSecrets(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "pimbackup.json")
	contents := `{"accounts":[{"id":"personal","host":"imap.example.test","username":"user","password":"secret-value"}]}`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--config", configPath, "config", "show"}, &stdout, &stderr, buildinfo.Info{})
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "secret-value") || !strings.Contains(stdout.String(), "<redacted>") {
		t.Fatalf("config show output = %q", stdout.String())
	}
}

func TestRestorableFlagsDropsServerManagedAndDestructiveFlags(t *testing.T) {
	flags := restorableFlags([]string{"\\Seen", "\\Recent", "\\Deleted", "custom"})
	if got, want := strings.Join(flags, ","), "\\Seen,custom"; got != want {
		t.Fatalf("restorableFlags() = %q, want %q", got, want)
	}
}

func TestHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"help"}, &stdout, &stderr, buildinfo.Info{})
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "db rebuild") {
		t.Fatalf("help output = %q", stdout.String())
	}
}
