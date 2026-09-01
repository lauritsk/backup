package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/pimbackup/model"
	runmodel "github.com/lauritsk/backup/internal/run"
)

func TestCatalogMailAndRunLifecycle(t *testing.T) {
	ctx := context.Background()
	catalog, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()

	if err := catalog.UpsertAccount(ctx, "personal", "imap"); err != nil {
		t.Fatal(err)
	}
	mailbox, err := catalog.EnsureMailbox(ctx, model.Mailbox{
		AccountID:      "personal",
		Name:           "INBOX",
		PathKey:        "INBOX--abc",
		Delimiter:      "/",
		UIDValidity:    12,
		RemoteMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	internalDate := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	message, err := catalog.PutMessage(ctx, model.Message{
		MailboxID:       mailbox.ID,
		UID:             3,
		InternalDate:    &internalDate,
		Size:            100,
		SHA256:          "abc",
		Path:            "accounts/personal/mail/3.eml",
		SidecarPath:     "accounts/personal/mail/3.json",
		Flags:           []string{"\\Seen"},
		Subject:         "Subject",
		From:            "sender@example.test",
		HeaderMessageID: "<id@example.test>",
		ArchivedAt:      internalDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID == 0 || message.AccountID != "personal" || message.UID != 3 {
		t.Fatalf("PutMessage() = %#v", message)
	}

	mailbox, err = catalog.GetMailbox(ctx, "personal", "INBOX", 12)
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.LastUID != 3 || mailbox.Messages != 1 {
		t.Fatalf("mailbox = %#v", mailbox)
	}
	if err := catalog.RewindMailbox(ctx, mailbox.ID, 3); err != nil {
		t.Fatal(err)
	}
	mailbox, _ = catalog.GetMailbox(ctx, "personal", "INBOX", 12)
	if mailbox.LastUID != 2 {
		t.Fatalf("LastUID = %d, want 2", mailbox.LastUID)
	}

	run, err := catalog.CreateRun(ctx, runmodel.OperationBackup, map[string]string{"account": "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runmodel.StatusQueued {
		t.Fatalf("new run status = %q", run.Status)
	}
	if err := catalog.StartRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := catalog.FinishRun(ctx, run.ID, runmodel.StatusSucceeded, nil, map[string]int{"fetched": 1}); err != nil {
		t.Fatal(err)
	}
	finished, err := catalog.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != runmodel.StatusSucceeded || finished.StartedAt == nil || finished.FinishedAt == nil {
		t.Fatalf("finished run = %#v", finished)
	}
	collection, err := catalog.EnsureCollection(ctx, model.Collection{AccountID: "personal", Kind: "contact", Name: "People", RemoteID: "people", RemoteURL: "https://example.test/people/"})
	if err != nil {
		t.Fatal(err)
	}
	object, err := catalog.PutObject(ctx, model.Object{CollectionID: collection.ID, RemoteID: "ada.vcf", ETag: "one", ContentType: "text/vcard", Size: 10, SHA256: "def", Path: "accounts/personal/contacts/ada.vcf", SidecarPath: "accounts/personal/contacts/ada.json", Title: "Ada", ArchivedAt: internalDate})
	if err != nil {
		t.Fatal(err)
	}
	if object.ID == 0 || object.Kind != "contact" || object.AccountID != "personal" {
		t.Fatalf("PutObject() = %#v", object)
	}
	objects, err := catalog.ListObjects(ctx, model.ObjectFilter{AccountID: "personal", Kind: "contact"})
	if err != nil || len(objects) != 1 {
		t.Fatalf("ListObjects() = %#v, %v", objects, err)
	}
	accounts, err := catalog.ListAccounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].Collections != 1 || accounts[0].Objects != 1 {
		t.Fatalf("ListAccounts() = %#v, %v", accounts, err)
	}
	if err := catalog.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMarkInterrupted(t *testing.T) {
	ctx := context.Background()
	catalog, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	run, err := catalog.CreateRun(ctx, runmodel.OperationVerify, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.StartRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	count, err := catalog.MarkInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("MarkInterrupted() = %d, want 1", count)
	}
	got, _ := catalog.GetRun(ctx, run.ID)
	if got.Status != runmodel.StatusInterrupted {
		t.Fatalf("run status = %q", got.Status)
	}
}

func TestGetMissingRecord(t *testing.T) {
	catalog, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	_, err = catalog.GetMessage(context.Background(), 999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMessage() error = %v", err)
	}
}
