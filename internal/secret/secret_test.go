package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDirectValue(t *testing.T) {
	got, set, err := Resolve(Source{
		Name:      "PIMBACKUP_PASSWORD",
		Direct:    "  password  ",
		DirectSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !set {
		t.Fatal("Resolve() reported an unset value")
	}
	if got != "  password  " {
		t.Fatalf("Resolve() = %q, want direct value unchanged", got)
	}
}

func TestResolveFileValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("password\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, set, err := Resolve(Source{
		Name:    "PIMBACKUP_PASSWORD",
		File:    path,
		FileSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !set {
		t.Fatal("Resolve() reported an unset value")
	}
	if got != "password" {
		t.Fatalf("Resolve() = %q, want %q", got, "password")
	}
}

func TestResolveRejectsBothSourcesEvenWhenEmpty(t *testing.T) {
	_, _, err := Resolve(Source{
		Name:      "PIMBACKUP_PASSWORD",
		DirectSet: true,
		FileSet:   true,
	})
	if err == nil {
		t.Fatal("Resolve() accepted direct and file values together")
	}
	if !strings.Contains(err.Error(), "PIMBACKUP_PASSWORD_FILE") {
		t.Fatalf("Resolve() error %q does not name the file setting", err)
	}
}

func TestResolveUnset(t *testing.T) {
	got, set, err := Resolve(Source{Name: "PIMBACKUP_PASSWORD"})
	if err != nil {
		t.Fatal(err)
	}
	if set || got != "" {
		t.Fatalf("Resolve() = %q, %t, want empty and unset", got, set)
	}
}

func TestResolveTrimsOnlyOneLineEnding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("password\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, _, err := Resolve(Source{File: path, FileSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "password\n" {
		t.Fatalf("Resolve() = %q, want one trailing newline preserved", got)
	}
}
