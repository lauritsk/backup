package appbackup

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/buildinfo"
)

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
