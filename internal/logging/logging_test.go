package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	var output bytes.Buffer
	logger, err := New("warn", "json", &output)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Error("visible")
	if strings.Contains(output.String(), "hidden") || !strings.Contains(output.String(), "visible") {
		t.Fatalf("output = %q", output.String())
	}
	if _, err := New("info", "invalid", &output); err == nil {
		t.Fatal("New accepted an invalid format")
	}
}
