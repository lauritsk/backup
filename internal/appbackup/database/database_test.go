package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/config"
)

func TestSQLiteDumpAndVerificationUseEmbeddedDriver(t *testing.T) {
	source := filepath.Join(t.TempDir(), "live's.sqlite")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE items (value TEXT); INSERT INTO items VALUES ('before')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o400); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	runner := Runner{DataDir: dataDir}
	database := config.DatabaseConfig{ID: "db", Type: "sqlite", Path: source, Timeout: config.Duration{Duration: time.Minute}}
	version, err := runner.Version(context.Background(), database)
	if err != nil || !strings.Contains(version, "modernc.org/sqlite") {
		t.Fatalf("Version = %q, %v", version, err)
	}
	if err := runner.Check(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(dataDir, "staging", "point", "database.sqlite")
	if err := runner.Dump(context.Background(), database, dump); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dump)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("dump mode = %v", info.Mode().Perm())
	}
	status, err := runner.VerifyDump(context.Background(), database, dump)
	if err != nil || status != "passed" {
		t.Fatalf("VerifyDump = %q, %v", status, err)
	}
	copyDB, err := sql.Open("sqlite", dump)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var value string
	if err := copyDB.QueryRow(`SELECT value FROM items`).Scan(&value); err != nil || value != "before" {
		t.Fatalf("snapshot value = %q, %v", value, err)
	}
}

func TestMySQLVersionDoesNotRequireRestoreClient(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mysqldump")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'mysqldump 8.4'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	database := config.DatabaseConfig{Type: "mysql", Binary: binary}
	version, err := (Runner{}).Version(context.Background(), database)
	if err != nil || version != "mysqldump 8.4" {
		t.Fatalf("Version = %q, %v", version, err)
	}
}

func TestPostgreSQLDumpUsesPasswordEnvironment(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "pg-tool")
	capture := filepath.Join(t.TempDir(), "capture")
	script := "#!/bin/sh\nprintf '%s\\n' \"$PGPASSWORD|$*\" >> '" + capture + "'\nif [ \"$1\" = '--version' ]; then echo 'pg_dump 17'; exit 0; fi\nif [ \"$1\" = '--list' ]; then exit 0; fi\nprintf 'native dump'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	database := config.DatabaseConfig{ID: "db", Type: "postgresql", Binary: binary, RestoreBinary: binary, Host: "database", User: "backup", Name: "app", ResolvedPassword: "secret", Timeout: config.Duration{Duration: time.Minute}}
	dataDir := t.TempDir()
	runner := Runner{DataDir: dataDir}
	version, err := runner.Version(context.Background(), database)
	if err != nil || version != "pg_dump 17" {
		t.Fatalf("Version = %q, %v", version, err)
	}
	dump := filepath.Join(dataDir, "staging", "point", "database.dump")
	if err := runner.Dump(context.Background(), database, dump); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(dump)
	if err != nil || string(contents) != "native dump" {
		t.Fatalf("dump = %q, %v", contents, err)
	}
	if err := runner.Dump(context.Background(), database, filepath.Join(t.TempDir(), "outside.dump")); err == nil {
		t.Fatal("Dump accepted a destination outside staging")
	}
	status, err := runner.VerifyDump(context.Background(), database, dump)
	if err != nil || status != "unknown" {
		t.Fatalf("VerifyDump = %q, %v", status, err)
	}
	calls, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "secret|") {
		t.Fatalf("calls = %q", calls)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 || strings.Contains(parts[1], "secret") {
			t.Fatalf("secret appeared in arguments: %q", line)
		}
	}
}
