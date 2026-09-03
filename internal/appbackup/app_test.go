package appbackup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/buildinfo"
)

func TestConfigValidateDoesNotRunExternalCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"restic":{"password":"secret","binary":"definitely-not-installed"},"applications":[{"id":"site","paths":["/srv/site"]}]}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", path, "config", "validate"}, &stdout, &stderr, buildinfo.Info{})
	if code != 0 || !strings.Contains(stdout.String(), "configuration is valid") {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, &stdout, &stderr, buildinfo.Info{Version: "test"})

	if code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "appbackup version=test ") {
		t.Fatalf("Run() output = %q", stdout.String())
	}
}
