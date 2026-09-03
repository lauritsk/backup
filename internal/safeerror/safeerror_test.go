package safeerror

import (
	"errors"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	if Clean(nil) != nil {
		t.Fatal("Clean(nil) is not nil")
	}
	got := Clean(errors.New("first\r\nsecond")).Error()
	if got != "first  second" {
		t.Fatalf("Clean line breaks = %q", got)
	}
	got = Clean(errors.New(strings.Repeat("x", 2100))).Error()
	if len(got) != 2003 || !strings.HasSuffix(got, "...") {
		t.Fatalf("Clean long error length = %d", len(got))
	}
}
