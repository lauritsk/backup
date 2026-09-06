package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestDefaultsUseTextLogs(t *testing.T) {
	if got := defaults().Log.Format; got != "text" {
		t.Fatalf("default log format = %q", got)
	}
}

func TestNormalizeOnlyConfiguresRequiredDatabaseClients(t *testing.T) {
	cfg := defaults()
	cfg.Applications = []ApplicationConfig{{Databases: []DatabaseConfig{
		{Type: "sqlite"},
		{Type: "mysql"},
		{Type: "mariadb"},
		{Type: "postgresql"},
	}}}
	normalize(&cfg)
	databases := cfg.Applications[0].Databases
	if databases[0].Binary != "" || databases[0].RestoreBinary != "" {
		t.Fatalf("SQLite clients = %#v", databases[0])
	}
	if databases[1].Binary != "mysqldump" || databases[1].RestoreBinary != "" {
		t.Fatalf("MySQL clients = %#v", databases[1])
	}
	if databases[2].Binary != "mariadb-dump" || databases[2].RestoreBinary != "" {
		t.Fatalf("MariaDB clients = %#v", databases[2])
	}
	if databases[3].Binary != "pg_dump" || databases[3].RestoreBinary != "pg_restore" {
		t.Fatalf("PostgreSQL clients = %#v", databases[3])
	}
}

func TestValidateDatabaseDoesNotRequireSQLiteOrMySQLRestoreBinaries(t *testing.T) {
	minute := Duration{Duration: time.Minute}
	for _, database := range []DatabaseConfig{
		{ID: "sqlite", Type: "sqlite", Path: "/srv/app.sqlite", Timeout: minute},
		{ID: "mysql", Type: "mysql", Binary: "mysqldump", Name: "app", Timeout: minute},
	} {
		if err := validateDatabase("/data", database); err != nil {
			t.Fatalf("validateDatabase(%s): %v", database.Type, err)
		}
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

func TestEffectiveVerificationCommandReturnsIndependentCopy(t *testing.T) {
	cfg := Config{}
	database := DatabaseConfig{VerifyCommand: &CommandConfig{Binary: "verify-dump", Args: []string{"{dump}"}}}
	command := cfg.EffectiveVerificationCommand(database)
	command.Args[0] = "changed"
	if database.VerifyCommand.Args[0] != "{dump}" {
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
