package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsUnknownAndDuplicateFields(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown":   `{"unknown":true}`,
		"duplicate": `{"data_dir":"/data","data_dir":"/tmp"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(Overrides{ConfigPath: &path})
			if err == nil {
				t.Fatal("Load succeeded")
			}
		})
	}
}

func TestRedactedCopyHidesAPISecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"server":{"auth_token":"api-secret"}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(Overrides{ConfigPath: &path})
	if err != nil {
		t.Fatal(err)
	}
	redacted := cfg.RedactedCopy()
	if redacted.Server.AuthToken == nil || *redacted.Server.AuthToken != Redacted {
		t.Fatalf("API token was not redacted: %#v", redacted.Server.AuthToken)
	}
	if strings.Contains(redacted.Server.ResolvedAuthToken, "secret") {
		t.Fatal("resolved secret remains in redacted copy")
	}
}

func TestValidateRejectsLocalSource(t *testing.T) {
	cfg := defaults()
	cfg.Sources = []SourceConfig{{ID: "local", Remote: "/tmp/files"}}
	normalize(&cfg)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rclone remote") {
		t.Fatalf("Validate error = %v", err)
	}
}
