package rclone

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/cloudbackup/config"
)

func TestRemoteObjectRejectsUnsafePaths(t *testing.T) {
	for _, path := range []string{"", "/absolute", "../escape", "directory/../file", `directory\file`, "bad\nname"} {
		if _, err := remoteObject("remote:source", path); err == nil {
			t.Errorf("remoteObject accepted %q", path)
		}
	}
	value, err := remoteObject("remote:source", "directory/file.txt")
	if err != nil || value != "remote:source/directory/file.txt" {
		t.Fatalf("remoteObject = %q, %v", value, err)
	}
}

func TestSourceContextParsesFilters(t *testing.T) {
	_, err := sourceContext(context.Background(), config.SourceConfig{Include: []string{"*.txt"}, Exclude: []string{"private/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceContext(context.Background(), config.SourceConfig{Include: []string{"["}}); err == nil {
		t.Fatal("sourceContext accepted an invalid filter")
	}
}

func TestEmbeddedRunnerAcquiresFromConfiguredRemote(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skip.bin"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(configPath, []byte("[test]\ntype = local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := Runner{ConfigPath: configPath}
	source := config.SourceConfig{Remote: "test:" + filepath.ToSlash(root), Include: []string{"*.txt"}}
	if err := runner.CheckSource(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	files, err := runner.Inventory(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "keep.txt" || files[0].Size != 8 {
		t.Fatalf("inventory = %#v", files)
	}
	var downloaded bytes.Buffer
	if err := runner.Download(context.Background(), source, files[0].Path, &downloaded); err != nil {
		t.Fatal(err)
	}
	if downloaded.String() != "contents" {
		t.Fatalf("download = %q", downloaded.String())
	}
}

func TestLimitedReader(t *testing.T) {
	reader := newLimitedReader(context.Background(), strings.NewReader("contents"), 1<<20)
	contents, err := io.ReadAll(reader)
	if err != nil || string(contents) != "contents" {
		t.Fatalf("ReadAll = %q, %v", contents, err)
	}
	if value, err := bandwidthBytes("20M/s"); err != nil || value != 20*(1<<20) {
		t.Fatalf("bandwidthBytes = %d, %v", value, err)
	}
}
