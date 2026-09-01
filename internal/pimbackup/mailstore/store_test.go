package mailstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/pimbackup/model"
)

const testMessage = "From: Sender <sender@example.test>\r\n" +
	"To: receiver@example.test\r\n" +
	"Date: Tue, 02 Jan 2024 03:04:05 +0000\r\n" +
	"Message-ID: <one@example.test>\r\n" +
	"Subject: A test message\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=boundary\r\n" +
	"\r\n" +
	"--boundary\r\nContent-Type: text/plain\r\n\r\nhello\r\n--boundary--\r\n"

func TestSaveVerifyAndScan(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mailbox := model.Mailbox{
		ID:          7,
		AccountID:   "personal",
		Name:        "Archive/2024",
		PathKey:     PathKey("Archive/2024"),
		Delimiter:   "/",
		UIDValidity: 42,
	}
	internalDate := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	saved, err := store.Save(context.Background(), mailbox, FetchedMessage{
		UID:          9,
		InternalDate: &internalDate,
		Flags:        []string{"\\Seen"},
		ExpectedSize: int64(len(testMessage)),
		Body:         strings.NewReader(testMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Created {
		t.Fatal("first save was not marked created")
	}
	if saved.Message.Subject != "A test message" || saved.Message.HeaderMessageID != "<one@example.test>" {
		t.Fatalf("parsed message = %#v", saved.Message)
	}
	if err := store.Verify(context.Background(), saved.Message); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	file, err := store.OpenMessage(saved.Message)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(file.Name())
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, []byte(testMessage)) {
		t.Fatal("stored message bytes changed")
	}

	again, err := store.Save(context.Background(), mailbox, FetchedMessage{
		UID:          9,
		InternalDate: &internalDate,
		Flags:        []string{"\\Seen"},
		ExpectedSize: int64(len(testMessage)),
		Body:         strings.NewReader(testMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created {
		t.Fatal("idempotent save was marked created")
	}
	if !again.Message.ArchivedAt.Equal(saved.Message.ArchivedAt) {
		t.Fatalf("ArchivedAt changed from %s to %s", saved.Message.ArchivedAt, again.Message.ArchivedAt)
	}

	scan := store.Scan(context.Background())
	if len(scan.Errors) != 0 {
		t.Fatalf("Scan() errors = %v", scan.Errors)
	}
	if len(scan.Mailboxes) != 1 || len(scan.Mailboxes[0].Messages) != 1 {
		t.Fatalf("Scan() = %#v", scan)
	}
}

func TestRecoverMissingSidecarFromCatalog(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mailbox := model.Mailbox{ID: 7, AccountID: "personal", Name: "INBOX", PathKey: PathKey("INBOX"), UIDValidity: 42}
	internalDate := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	saved, err := store.Save(context.Background(), mailbox, FetchedMessage{
		UID: 9, InternalDate: &internalDate, Flags: []string{"\\Seen"}, ExpectedSize: int64(len(testMessage)), Body: strings.NewReader(testMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Message.ID = 11
	sidecar, err := store.Resolve(saved.Message.SidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}

	recovered, problems := store.RecoverMissingSidecars(context.Background(), []model.Message{saved.Message})
	if len(problems) != 0 || len(recovered) != 1 {
		t.Fatalf("RecoverMissingSidecars() = %v, %v", recovered, problems)
	}
	scan := store.Scan(context.Background())
	if len(scan.Errors) != 0 || len(scan.Mailboxes) != 1 || len(scan.Mailboxes[0].Messages) != 1 {
		t.Fatalf("Scan() after recovery = %#v", scan)
	}
	message := scan.Mailboxes[0].Messages[0]
	if !message.InternalDate.Equal(internalDate) || len(message.Flags) != 1 || message.Flags[0] != "\\Seen" {
		t.Fatalf("recovered catalog metadata = %#v", message)
	}
}

func TestRecoverMissingSidecarWithoutCatalog(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mailbox := model.Mailbox{ID: 7, AccountID: "personal", Name: "INBOX", PathKey: PathKey("INBOX"), UIDValidity: 42}
	saved, err := store.Save(context.Background(), mailbox, FetchedMessage{
		UID: 9, ExpectedSize: int64(len(testMessage)), Body: strings.NewReader(testMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := store.Resolve(saved.Message.SidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}

	recovered, problems := store.RecoverMissingSidecars(context.Background(), nil)
	if len(problems) != 0 || len(recovered) != 1 {
		t.Fatalf("RecoverMissingSidecars() = %v, %v", recovered, problems)
	}
	metadata, err := readMessageMetadata(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.Recovered || metadata.UID != 9 || metadata.SHA256 != saved.Message.SHA256 || metadata.Size != saved.Message.Size {
		t.Fatalf("recovered metadata = %#v", metadata)
	}
	scan := store.Scan(context.Background())
	if len(scan.Errors) != 0 || len(scan.Mailboxes) != 1 || len(scan.Mailboxes[0].Messages) != 1 {
		t.Fatalf("Scan() after recovery = %#v", scan)
	}
}

func TestRecoveryDoesNotTrustTamperedKnownPayload(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mailbox := model.Mailbox{ID: 7, AccountID: "personal", Name: "INBOX", PathKey: PathKey("INBOX"), UIDValidity: 42}
	saved, err := store.Save(context.Background(), mailbox, FetchedMessage{
		UID: 9, ExpectedSize: int64(len(testMessage)), Body: strings.NewReader(testMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Message.ID = 11
	messagePath, _ := store.Resolve(saved.Message.Path)
	sidecarPath, _ := store.Resolve(saved.Message.SidecarPath)
	if err := os.Remove(sidecarPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messagePath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	recovered, problems := store.RecoverMissingSidecars(context.Background(), []model.Message{saved.Message})
	if len(recovered) != 0 || len(problems) == 0 {
		t.Fatalf("RecoverMissingSidecars() = %v, %v", recovered, problems)
	}
	if _, err := os.Lstat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("sidecar was recreated for a tampered payload: %v", err)
	}
}

func TestSaveDoesNotOverwriteIdentityConflict(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mailbox := model.Mailbox{ID: 1, AccountID: "personal", Name: "INBOX", PathKey: PathKey("INBOX"), UIDValidity: 1}
	first, err := store.Save(context.Background(), mailbox, FetchedMessage{
		UID: 1, ExpectedSize: int64(len(testMessage)), Body: strings.NewReader(testMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(testMessage, "hello", "changed", 1)
	_, err = store.Save(context.Background(), mailbox, FetchedMessage{
		UID: 1, ExpectedSize: int64(len(other)), Body: strings.NewReader(other),
	})
	if err == nil || !strings.Contains(err.Error(), "identity conflict") {
		t.Fatalf("Save() error = %v", err)
	}
	file, err := store.OpenMessage(first.Message)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(file.Name())
	file.Close()
	if string(contents) != testMessage {
		t.Fatal("identity conflict overwrote original payload")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(file.Name()), "1.conflict-*.eml"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("conflict files = %v, %v", matches, err)
	}
}

func TestResolveRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dataDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve("escape/message.eml"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Resolve() error = %v", err)
	}
}
