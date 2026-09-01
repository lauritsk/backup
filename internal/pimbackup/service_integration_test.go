package pimbackup

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/model"
	runmodel "github.com/lauritsk/backup/internal/run"
)

func TestIMAPBackupVerifyAndRestore(t *testing.T) {
	const username = "user"
	const password = "password"
	raw := []byte("From: sender@example.test\r\nTo: receiver@example.test\r\nDate: Tue, 02 Jan 2024 03:04:05 +0000\r\nMessage-ID: <integration@example.test>\r\nSubject: Integration\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n")

	memoryServer := imapmemserver.New()
	user := imapmemserver.NewUser(username, password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	internalDate := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := user.Append("INBOX", newTestLiteral(raw), &goimap.AppendOptions{
		Flags: []goimap.Flag{goimap.FlagSeen},
		Time:  internalDate,
	}); err != nil {
		t.Fatal(err)
	}
	memoryServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memoryServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1: {},
			goimap.CapUIDPlus:   {},
		},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("server.Close() = %v", err)
		}
		if err := <-serveDone; err != nil {
			t.Errorf("server.Serve() = %v", err)
		}
	})

	address := listener.Addr().(*net.TCPAddr)
	secret := password
	dataDir := t.TempDir()
	cfg := config.Config{
		DataDir: dataDir,
		Accounts: []config.AccountConfig{{
			ID:               "personal",
			Protocol:         "imap",
			Host:             address.IP.String(),
			Port:             address.Port,
			TLS:              "plain",
			AllowInsecure:    true,
			Username:         username,
			Password:         &secret,
			ResolvedPassword: password,
			Mailboxes:        []string{"INBOX"},
			Timeout:          config.Duration{Duration: 5 * time.Second},
		}},
	}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceOpen := true
	defer func() {
		if serviceOpen {
			_ = service.Close()
		}
	}()

	backupRun, err := service.Backup(context.Background(), model.BackupRequest{})
	if err != nil {
		t.Fatalf("Backup() = %v, run = %#v", err, backupRun)
	}
	if backupRun.Status != runmodel.StatusSucceeded {
		t.Fatalf("backup status = %q", backupRun.Status)
	}
	messages, err := service.ListMessages(context.Background(), model.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	message := messages[0]
	if message.Subject != "Integration" || message.SHA256 == "" || message.Size != int64(len(raw)) {
		t.Fatalf("message = %#v", message)
	}
	_, file, err := service.OpenMessage(context.Background(), message.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, raw) {
		t.Fatal("stored RFC822 message differs from source")
	}

	secondRun, err := service.Backup(context.Background(), model.BackupRequest{})
	if err != nil {
		t.Fatalf("second Backup() = %v, run = %#v", err, secondRun)
	}
	messages, _ = service.ListMessages(context.Background(), model.MessageFilter{Limit: 10})
	if len(messages) != 1 {
		t.Fatalf("second backup produced %d messages", len(messages))
	}

	verifyRun, err := service.Verify(context.Background(), model.VerifyRequest{})
	if err != nil {
		t.Fatalf("Verify() = %v, run = %#v", err, verifyRun)
	}
	if verifyRun.Status != runmodel.StatusSucceeded {
		t.Fatalf("verify status = %q", verifyRun.Status)
	}

	restoreRun, err := service.Restore(context.Background(), model.RestoreRequest{
		MessageIDs:    []int64{message.ID},
		TargetAccount: "personal",
		TargetMailbox: "Restored",
		CreateMailbox: true,
		Confirm:       true,
	})
	if err != nil {
		t.Fatalf("Restore() = %v, run = %#v", err, restoreRun)
	}
	status, err := user.Status("Restored", &goimap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.NumMessages == nil || *status.NumMessages != 1 {
		t.Fatalf("restored mailbox status = %#v", status)
	}

	rebuild, err := service.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("Rebuild() = %v, report = %#v", err, rebuild)
	}
	if rebuild.Mailboxes != 1 || rebuild.Messages != 1 {
		t.Fatalf("rebuild report = %#v", rebuild)
	}

	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	serviceOpen = false
	preserved, err := OpenService(context.Background(), cfg, ServiceOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	preservedMailboxes, err := preserved.ListMailboxes(context.Background(), "personal", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(preservedMailboxes) != 1 || preservedMailboxes[0].LastUID != 0 {
		t.Fatalf("rebuilt mailbox state after restart = %#v", preservedMailboxes)
	}
	if err := preserved.Close(); err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dataDir, "pim.db") + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	recovered, err := OpenService(context.Background(), cfg, ServiceOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	recoveredMessages, err := recovered.ListMessages(context.Background(), model.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveredMessages) != 1 || recoveredMessages[0].SHA256 != message.SHA256 {
		t.Fatalf("messages recovered without SQLite = %#v", recoveredMessages)
	}
	recoveredMailboxes, err := recovered.ListMailboxes(context.Background(), "personal", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveredMailboxes) != 1 || recoveredMailboxes[0].LastUID != 0 {
		t.Fatalf("recovered mailbox state = %#v", recoveredMailboxes)
	}
	if _, err := recovered.Backup(context.Background(), model.BackupRequest{}); err != nil {
		t.Fatalf("backup after SQLite recovery = %v", err)
	}
}

func TestCardDAVBackupVerifyRestoreAndRebuild(t *testing.T) {
	const card = "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ada Lovelace\r\nEND:VCARD\r\n"
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(request.Body)
		switch {
		case request.Method == "PROPFIND" && strings.Contains(string(body), "current-user-principal"):
			writeDAVMultiStatus(writer, `<d:response><d:href>/people/</d:href><d:propstat><d:prop><d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && strings.Contains(string(body), "addressbook-home-set"):
			writeDAVMultiStatus(writer, `<d:response><d:href>/principal/</d:href><d:propstat><d:prop><card:addressbook-home-set><d:href>/people/</d:href></card:addressbook-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && strings.Contains(string(body), "addressbook-description"):
			writeDAVMultiStatus(writer, `<d:response><d:href>/people/</d:href><d:propstat><d:prop><d:displayname>People</d:displayname><d:resourcetype><d:collection/><card:addressbook/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND":
			writeDAVMultiStatus(writer, `<d:response><d:href>/people/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype><d:getlastmodified>Tue, 02 Jan 2024 03:04:05 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/people/ada.vcf</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>62</d:getcontentlength><d:getcontenttype>text/vcard</d:getcontenttype><d:getetag>"one"</d:getetag><d:getlastmodified>Tue, 02 Jan 2024 03:04:05 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == http.MethodGet && request.URL.Path == "/people/ada.vcf":
			gets.Add(1)
			writer.Header().Set("Content-Type", "text/vcard")
			_, _ = io.WriteString(writer, card)
		case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/people/pimbackup-"):
			if string(body) != card {
				t.Errorf("restored card = %q", body)
			}
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	dataDir := t.TempDir()
	cfg := config.Config{DataDir: dataDir, Accounts: []config.AccountConfig{{ID: "contacts", Protocol: "carddav", URL: server.URL + "/people/", Auth: "bearer", ResolvedToken: "token", Collections: []string{"*"}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if run, err := service.Backup(context.Background(), model.BackupRequest{}); err != nil || run.Status != runmodel.StatusSucceeded {
		t.Fatalf("Backup() = %#v, %v", run, err)
	}
	objects, err := service.ListObjects(context.Background(), model.ObjectFilter{Limit: 10})
	if err != nil || len(objects) != 1 || objects[0].Title != "Ada Lovelace" {
		t.Fatalf("objects = %#v, %v", objects, err)
	}
	if _, err := service.Backup(context.Background(), model.BackupRequest{}); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 1 {
		t.Fatalf("unchanged contact downloaded %d times", gets.Load())
	}
	if _, err := service.Verify(context.Background(), model.VerifyRequest{ObjectID: objects[0].ID}); err != nil {
		t.Fatal(err)
	}
	restoreRun, err := service.Restore(context.Background(), model.RestoreRequest{ObjectIDs: []int64{objects[0].ID}, TargetAccount: "contacts", TargetCollection: "People", Confirm: true})
	if err != nil {
		t.Fatalf("Restore() = %#v, %v", restoreRun, err)
	}
	report, err := service.Rebuild(context.Background())
	if err != nil || report.Collections != 1 || report.Objects != 1 {
		t.Fatalf("Rebuild() = %#v, %v", report, err)
	}
}

func writeDAVMultiStatus(writer http.ResponseWriter, responses string) {
	writer.Header().Set("Content-Type", "application/xml; charset=utf-8")
	writer.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(writer, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">`+responses+`</d:multistatus>`)
}

type testLiteral struct {
	*bytes.Reader
	size int64
}

func newTestLiteral(value []byte) *testLiteral {
	return &testLiteral{Reader: bytes.NewReader(value), size: int64(len(value))}
}

func (literal *testLiteral) Size() int64 { return literal.size }
