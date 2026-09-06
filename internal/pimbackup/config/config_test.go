package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pimbackup.json")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPrecedenceAndSecretFile(t *testing.T) {
	dir := t.TempDir()
	passwordPath := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "pimbackup.json")
	contents := `{"data_dir":"/from-json","log":{"level":"warn","format":"text"},"accounts":[{"id":"personal","host":"imap.example.test","username":"user","password_file":` + quoteJSON(passwordPath) + `,"mailboxes":["INBOX"]}]}`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIMBACKUP_CONFIG", configPath)
	t.Setenv("PIMBACKUP_DATA_DIR", "/from-environment")
	t.Setenv("PIMBACKUP_LOG_LEVEL", "error")
	cliDataDir := t.TempDir()
	cfg, err := Load(Overrides{DataDir: &cliDataDir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != cliDataDir || cfg.Log.Level != "error" || cfg.Log.Format != "text" {
		t.Fatalf("effective config = %#v", cfg)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ResolvedPassword != "from-file" {
		t.Fatalf("accounts = %#v", cfg.Accounts)
	}
	account := cfg.Accounts[0]
	if account.Port != 993 || account.TLS != "implicit" || account.Timeout.Duration == 0 {
		t.Fatalf("account defaults = %#v", account)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestDefaultsUseTextLogs(t *testing.T) {
	if got := defaults().Log.Format; got != "text" {
		t.Fatalf("default log format = %q", got)
	}
}

func TestAccountEnvironmentSecretOverridesJSON(t *testing.T) {
	path := writeConfig(t, `{"accounts":[{"id":"my-account","host":"imap.example.test","username":"user","password":"from-json"}]}`)
	t.Setenv("PIMBACKUP_CONFIG", path)
	t.Setenv("PIMBACKUP_ACCOUNT_MY_ACCOUNT_PASSWORD", "from-environment")
	cfg, err := Load(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Accounts[0].ResolvedPassword; got != "from-environment" {
		t.Fatalf("ResolvedPassword = %q", got)
	}
}

func TestEnvironmentSecretDoesNotReadOverriddenJSONFile(t *testing.T) {
	path := writeConfig(t, `{"accounts":[{"id":"personal","host":"imap.example.test","username":"user","password_file":"/missing/lower-precedence-secret"}]}`)
	t.Setenv("PIMBACKUP_CONFIG", path)
	t.Setenv("PIMBACKUP_ACCOUNT_PERSONAL_PASSWORD", "from-environment")
	cfg, err := Load(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Accounts[0].ResolvedPassword; got != "from-environment" {
		t.Fatalf("ResolvedPassword = %q", got)
	}
}

func TestLoadRejectsAmbiguousEmptyEnvironmentSecret(t *testing.T) {
	path := writeConfig(t, `{"accounts":[{"id":"personal","host":"imap.example.test","username":"user","password":"from-json"}]}`)
	t.Setenv("PIMBACKUP_CONFIG", path)
	t.Setenv("PIMBACKUP_ACCOUNT_PERSONAL_PASSWORD", "")
	t.Setenv("PIMBACKUP_ACCOUNT_PERSONAL_PASSWORD_FILE", "")
	_, err := Load(Overrides{})
	if err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestDisabledAccountDoesNotReadSecretFile(t *testing.T) {
	path := writeConfig(t, `{"accounts":[{"id":"disabled","host":"imap.example.test","username":"user","password_file":"/missing/secret","disabled":true}]}`)
	cfg, err := Load(Overrides{ConfigPath: &path})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsDuplicateJSONField(t *testing.T) {
	path := writeConfig(t, `{"data_dir":"/data","data_dir":"/tmp"}`)
	if _, err := Load(Overrides{ConfigPath: &path}); err == nil {
		t.Fatal("Load() accepted duplicate JSON fields")
	}
}

func TestLoadRejectsUnknownJSONField(t *testing.T) {
	path := writeConfig(t, `{"unknown":true}`)
	_, err := Load(Overrides{ConfigPath: &path})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestValidateDataDirectory(t *testing.T) {
	cfg := defaults()
	cfg.DataDir = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	for _, invalid := range []string{"relative", "/"} {
		cfg.DataDir = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "clean absolute") {
			t.Fatalf("Validate(%q) error = %v", invalid, err)
		}
	}
}

func TestValidateAcceptsAutodiscoveredAndExplicitPIMAccounts(t *testing.T) {
	cfg := defaults()
	cfg.Accounts = []AccountConfig{
		{ID: "mail", Protocol: "jmap", Host: "mail.example", Auth: "bearer", ResolvedToken: "token", Timeout: Duration{Duration: time.Second}, Collections: []string{"*"}},
		{ID: "contacts", Protocol: "carddav", Username: "me@example.test", Auth: "basic", ResolvedPassword: "password", Timeout: Duration{Duration: time.Second}, Collections: []string{"*"}},
		{ID: "calendar", Protocol: "caldav", URL: "https://dav.example/calendars/me/", Auth: "basic", Username: "me", ResolvedPassword: "password", Timeout: Duration{Duration: time.Second}, Collections: []string{"*"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateRejectsUnsafeDiscoveryDomains(t *testing.T) {
	for _, account := range []AccountConfig{
		{ID: "mail", Protocol: "jmap", Host: "mail.example:443", Auth: "bearer", ResolvedToken: "token", Timeout: Duration{Duration: time.Second}},
		{ID: "contacts", Protocol: "carddav", Username: "me@example.test/path", Auth: "basic", ResolvedPassword: "password", Timeout: Duration{Duration: time.Second}},
	} {
		if err := validateAccount(account); err == nil || !strings.Contains(err.Error(), "autodiscovery host") {
			t.Errorf("validateAccount(%#v) = %v", account, err)
		}
	}
}

func TestValidateRejectsInsecureHTTPPIMAccount(t *testing.T) {
	account := AccountConfig{ID: "mail", Protocol: "jmap", URL: "http://mail.example/jmap", Auth: "bearer", ResolvedToken: "token", Timeout: Duration{Duration: time.Second}, Collections: []string{"*"}}
	if err := validateAccount(account); err == nil || !strings.Contains(err.Error(), "allow_insecure") {
		t.Fatalf("validateAccount() = %v", err)
	}
}

func TestValidateRequiresExplicitInsecureTLSAcknowledgement(t *testing.T) {
	account := AccountConfig{ID: "mail", Protocol: "imap", Host: "mail.example", Port: 993, TLS: "implicit", Username: "user", ResolvedPassword: "password", InsecureSkipVerify: true, Timeout: Duration{Duration: time.Second}, Mailboxes: []string{"*"}}
	if err := validateAccount(account); err == nil || !strings.Contains(err.Error(), "insecure_skip_verify requires") {
		t.Fatalf("validateAccount() = %v", err)
	}
	account.AllowInsecure = true
	if err := validateAccount(account); err != nil {
		t.Fatalf("validateAccount() with acknowledgement = %v", err)
	}
}

func TestRedactedCopyEncodesNoSecret(t *testing.T) {
	secretValue := "very-secret"
	cfg := defaults()
	cfg.Accounts = []AccountConfig{{ID: "personal", Protocol: "imap", Host: "imap.example.test", Port: 993, TLS: "implicit", Username: "user", Password: &secretValue, ResolvedPassword: secretValue, Mailboxes: []string{"*"}, Timeout: Duration{Duration: time.Second}}}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(cfg.RedactedCopy()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secretValue) || !strings.Contains(output.String(), Redacted) {
		t.Fatalf("redacted output = %s", output.String())
	}
}

func quoteJSON(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
