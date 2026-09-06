package cli

import (
	"slices"
	"testing"
)

func TestExtractGlobalsAcceptsOptionsBeforeAndAfterCommand(t *testing.T) {
	globals, remaining, err := ExtractGlobals([]string{"--json", "backup", "--application", "site", "--data-dir=/var/lib/backup", "--log-format", "text"})
	if err != nil {
		t.Fatal(err)
	}
	if !globals.JSON || globals.DataDir == nil || *globals.DataDir != "/var/lib/backup" || globals.LogFormat == nil || *globals.LogFormat != "text" {
		t.Fatalf("globals = %#v", globals)
	}
	if !slices.Equal(remaining, []string{"backup", "--application", "site"}) {
		t.Fatalf("remaining = %#v", remaining)
	}
}

func TestExtractGlobalsRejectsMissingAndInvalidValues(t *testing.T) {
	for _, arguments := range [][]string{{"backup", "--config"}, {"--json=maybe", "status"}, {"--data-dir="}} {
		if _, _, err := ExtractGlobals(arguments); err == nil {
			t.Errorf("ExtractGlobals(%q) succeeded", arguments)
		}
	}
}
