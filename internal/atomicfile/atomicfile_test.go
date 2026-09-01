package atomicfile

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesAndReplacesFile(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "nested", "value")
	for _, value := range []string{"first", "second"} {
		err := Write(filename, 0o600, func(writer io.Writer) error {
			_, err := io.WriteString(writer, value)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != value {
			t.Fatalf("contents = %q, want %q", contents, value)
		}
	}
}

func TestWriteDoesNotReplaceOnWriterError(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(filename, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	wantErr := io.ErrUnexpectedEOF
	err := Write(filename, 0o600, func(io.Writer) error { return wantErr })
	if err != wantErr {
		t.Fatalf("Write() error = %v, want %v", err, wantErr)
	}
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original" {
		t.Fatalf("contents = %q, want original", contents)
	}
}
