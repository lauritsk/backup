package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/appbackup/config"
)

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
