package processenv

import (
	"strings"
	"testing"
)

func TestWithoutRemovesEveryDuplicateOfNamedVariables(t *testing.T) {
	environ := []string{"PATH=/bin", "RESTIC_PASSWORD=one", "restic_password=two", "RESTIC_PASSWORD=three", "OTHER=value"}
	got := without(environ, nil, "RESTIC_PASSWORD")
	joined := strings.Join(got, "\n")
	if strings.Contains(strings.ToUpper(joined), "RESTIC_PASSWORD=") {
		t.Fatalf("without() retained secret variables: %q", got)
	}
	if joined != "PATH=/bin\nOTHER=value" {
		t.Fatalf("without() = %q", got)
	}
}

func TestWithoutPrefixesRemovesCredentialFamilies(t *testing.T) {
	environ := []string{"PATH=/bin", "RCLONE_CONFIG_REMOTE_TOKEN=secret", "OTHER=value"}
	got := without(environ, []string{"RCLONE_CONFIG_"})
	if joined := strings.Join(got, "\n"); joined != "PATH=/bin\nOTHER=value" {
		t.Fatalf("without() = %q", got)
	}
}
