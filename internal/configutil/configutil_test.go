package configutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeFileRejectsInvalidObjectFields(t *testing.T) {
	type value struct {
		Name string `json:"name"`
	}
	for name, contents := range map[string]string{
		"duplicate": `{"name":"first","name":"second"}`,
		"unknown":   `{"other":true}`,
		"trailing":  `{"name":"first"} {"name":"second"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := DecodeFile(path, false, 1<<20, new(value)); err == nil {
				t.Fatal("DecodeFile succeeded")
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	var duration Duration
	if err := duration.UnmarshalText([]byte("15m")); err != nil {
		t.Fatal(err)
	}
	if duration.Duration != 15*time.Minute {
		t.Fatalf("duration = %s", duration.Duration)
	}
	encoded, err := duration.MarshalText()
	if err != nil || string(encoded) != "15m0s" {
		t.Fatalf("MarshalText = %q, %v", encoded, err)
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("CONFIGUTIL_TEST_BOOL", "not-a-bool")
	var target bool
	if err := EnvBool("CONFIGUTIL_TEST_BOOL", &target); err == nil || !strings.Contains(err.Error(), "CONFIGUTIL_TEST_BOOL") {
		t.Fatalf("EnvBool error = %v", err)
	}
}
