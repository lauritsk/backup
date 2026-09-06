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
	got = New("exact-secret").Clean(errors.New("request https://user:pass@example.test/path?token=query failed with Bearer abc and password=exact-secret")).Error()
	for _, leaked := range []string{"user:", ":pass@", "query", "abc", "exact-secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("Clean leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "https://example.test/path") {
		t.Fatalf("Clean URL = %q", got)
	}
	got = New("token/with spaces").Clean(errors.New("path token%2Fwith%20spaces query token%2Fwith+spaces")).Error()
	if strings.Contains(got, "token%2Fwith") {
		t.Fatalf("Clean leaked an encoded secret in %q", got)
	}
}
