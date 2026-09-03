package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValidateAndRedact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{
  "restic":{"password":"repository-secret"},
  "applications":[{"id":"site","paths":["/srv/site"],"databases":[{"id":"db","type":"postgresql","name":"site","password":"database-secret"}]}]
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(Overrides{ConfigPath: &path})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Restic.ResolvedPassword != "repository-secret" || cfg.Applications[0].Databases[0].ResolvedPassword != "database-secret" {
		t.Fatal("secrets were not resolved")
	}
	redacted := cfg.RedactedCopy()
	if deref(redacted.Restic.Password) != Redacted || deref(redacted.Applications[0].Databases[0].Password) != Redacted {
		t.Fatalf("redacted config = %#v", redacted)
	}
}

func TestValidateRejectsPathOverlappingData(t *testing.T) {
	cfg := defaults()
	cfg.Restic.ResolvedPassword = "secret"
	cfg.Applications = []ApplicationConfig{{ID: "bad", Paths: []string{"/"}, Timeout: duration(1)}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestEffectiveVerificationCommandUsesConfiguredEngine(t *testing.T) {
	cfg := Config{Engine: &EngineConfig{Type: "docker", Binary: "/usr/local/bin/docker", Socket: "/run/docker.sock"}}
	database := DatabaseConfig{VerifyCommand: &CommandConfig{Binary: "docker", Args: []string{"run", "{dump}"}}}
	command := cfg.EffectiveVerificationCommand(database)
	if command.Binary != cfg.Engine.Binary || strings.Join(command.Args, " ") != "--host unix:///run/docker.sock run {dump}" {
		t.Fatalf("effective command = %#v", command)
	}
	if database.VerifyCommand.Binary != "docker" || len(database.VerifyCommand.Args) != 2 {
		t.Fatal("effective command mutated configuration")
	}
}

func TestLoadRejectsAmbiguousSecret(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, []byte("file-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	contents := `{"restic":{"password":"direct","password_file":` + strconvQuote(secretPath) + `}}`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Overrides{ConfigPath: &configPath}); err == nil {
		t.Fatal("Load accepted direct and file secrets")
	}
}

func strconvQuote(value string) string { return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"` }
