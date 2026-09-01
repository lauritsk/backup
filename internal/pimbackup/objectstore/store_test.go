package objectstore

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/pimbackup/model"
)

const contact = "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ada Lovelace\r\nEND:VCARD\r\n"

func TestSaveVerifyAndScanContact(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	collection := model.Collection{ID: 7, AccountID: "personal", Kind: "contact", Name: "People", RemoteID: "https://example.test/addressbook/", RemoteURL: "https://example.test/addressbook/"}
	saved, err := store.Save(context.Background(), collection, "https://example.test/addressbook/ada.vcf", "one", "text/vcard", Attributes{}, strings.NewReader(contact))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Created || saved.Object.Title != "Ada Lovelace" {
		t.Fatalf("Save() = %#v", saved)
	}
	if err := store.Verify(context.Background(), saved.Object); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	file, err := store.Open(saved.Object)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file.Name())
	_ = file.Close()
	if string(data) != contact {
		t.Fatalf("stored payload changed: %q", data)
	}
	scan := store.Scan(context.Background())
	if len(scan.Errors) != 0 || len(scan.Collections) != 1 || len(scan.Collections[0].Objects) != 1 {
		t.Fatalf("Scan() = %#v", scan)
	}
}

func TestSaveVerifyAndScanCalendar(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calendar := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:event-1\r\nSUMMARY:Appointment\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	collection := model.Collection{ID: 8, AccountID: "personal", Kind: "calendar", Name: "Calendar", RemoteID: "calendar"}
	saved, err := store.Save(context.Background(), collection, "event.ics", "one", "text/calendar", Attributes{}, strings.NewReader(calendar))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Object.Title != "Appointment" {
		t.Fatalf("calendar title = %q", saved.Object.Title)
	}
	if err := store.Verify(context.Background(), saved.Object); err != nil {
		t.Fatal(err)
	}
}

func TestSaveRejectsInvalidStandardObject(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	collection := model.Collection{ID: 7, AccountID: "personal", Kind: "calendar", Name: "Calendar", RemoteID: "calendar"}
	if _, err := store.Save(context.Background(), collection, "event.ics", "one", "text/calendar", Attributes{}, strings.NewReader("not a calendar")); err == nil {
		t.Fatal("Save() accepted invalid iCalendar")
	}
	entries, err := os.ReadDir(store.accountsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("account data after failed save: %v, %v", entries, err)
	}
}

func TestSaveRepairsMissingPayload(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	collection := model.Collection{ID: 7, AccountID: "personal", Kind: "contact", Name: "People", RemoteID: "people"}
	saved, err := store.Save(context.Background(), collection, "ada.vcf", "one", "text/vcard", Attributes{}, strings.NewReader(contact))
	if err != nil {
		t.Fatal(err)
	}
	path, _ := store.Resolve(saved.Object.Path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	repaired, err := store.Save(context.Background(), collection, "ada.vcf", "one", "text/vcard", Attributes{}, strings.NewReader(contact))
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Created {
		t.Fatal("repair was reported as a new identity")
	}
	if err := store.Verify(context.Background(), repaired.Object); err != nil {
		t.Fatal(err)
	}
}
