package rclone

import (
	"path/filepath"
	"testing"
)

func TestValidateInvocationRejectsRemoteWrites(t *testing.T) {
	data := t.TempDir()
	tests := [][]string{
		{"sync", "remote:source", filepath.Join(data, "files")},
		{"copy", filepath.Join(data, "local"), "remote:destination"},
		{"delete", "remote:path"},
		{"copy", "remote:source", filepath.Join(data, "..", "escape")},
	}
	for _, args := range tests {
		if err := validateInvocation(data, args); err == nil {
			t.Errorf("validateInvocation(%q) accepted an unsafe command", args)
		}
	}
}

func TestValidateInvocationAcceptsAcquisition(t *testing.T) {
	data := t.TempDir()
	if err := validateInvocation(data, []string{"copy", "remote:source", filepath.Join(data, "sources", "one", "files")}); err != nil {
		t.Fatal(err)
	}
	if err := validateInvocation(data, []string{"lsf", "remote:source", "--max-depth", "1"}); err != nil {
		t.Fatal(err)
	}
}
