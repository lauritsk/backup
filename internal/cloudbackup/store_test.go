package cloudbackup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

func TestManifestHistoryPrecedesLatestPointer(t *testing.T) {
	dataDir := t.TempDir()
	store, err := newFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.prepareSource("documents"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.manifestsRoot("documents"), "latest.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := model.Manifest{SchemaVersion: 1, SourceID: "documents", RunID: "run-1"}
	if _, err := store.writeManifest(manifest); err == nil {
		t.Fatal("writeManifest() succeeded with an invalid latest pointer")
	}
	if _, err := os.Stat(filepath.Join(store.manifestsRoot("documents"), "run-1.json")); err != nil {
		t.Fatalf("historical manifest was not committed first: %v", err)
	}
}

func TestCommitValidatesBeforeAtomicPromotion(t *testing.T) {
	dataDir := t.TempDir()
	store, err := newFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.prepareSource("documents"); err != nil {
		t.Fatal(err)
	}
	remote := model.RemoteFile{Path: "report.txt", Size: 8, Hashes: map[string]string{"SHA-256": strings.Repeat("0", 64)}}
	if _, err := store.commit(context.Background(), "documents", remote, func(writer io.Writer) error {
		_, err := io.WriteString(writer, "original")
		return err
	}); err == nil {
		t.Fatal("commit accepted a mismatched remote hash")
	}
	if _, err := os.Stat(filepath.Join(store.filesRoot("documents"), "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("invalid payload was promoted: %v", err)
	}
}

func TestFileStoreRejectsSymlinkedSourceDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	dataDir := t.TempDir()
	store, err := newFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Symlink(t.TempDir(), filepath.Join(dataDir, "sources", "documents")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prepareSource("documents"); err == nil || !strings.Contains(err.Error(), "plain directory") {
		t.Fatalf("prepareSource() through symlink = %v", err)
	}
}
