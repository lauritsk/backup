package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.sr.ht/~rockorager/go-jmap/mail"

	"github.com/lauritsk/backup/internal/pimbackup/config"
)

func TestDiscoverEndpoint(t *testing.T) {
	for _, test := range []struct {
		account config.AccountConfig
		want    string
	}{
		{config.AccountConfig{Host: "mail.example.test"}, "https://mail.example.test/.well-known/jmap"},
		{config.AccountConfig{Username: "person@example.test"}, "https://example.test/.well-known/jmap"},
		{config.AccountConfig{URL: "https://custom.example/session"}, "https://custom.example/session"},
	} {
		got, err := discoverEndpoint(test.account)
		if err != nil || got != test.want {
			t.Errorf("discoverEndpoint(%#v) = %q, %v", test.account, got, err)
		}
	}
}

func TestSessionDiscoveryHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) { <-request.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := New(ctx, config.AccountConfig{Protocol: "jmap", AllowInsecure: true, URL: server.URL, Auth: "bearer", ResolvedToken: "token", Timeout: config.Duration{Duration: 5 * time.Second}})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("New() = %v after %v", err, time.Since(started))
	}
}

func TestAuthenticatedRedirectCannotLeaveApprovedOrigins(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("source Authorization = %q", request.Header.Get("Authorization"))
		}
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()
	transport, err := newAuthTransport(http.DefaultTransport, config.AccountConfig{Auth: "bearer", ResolvedToken: "secret"}, source.URL, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}
	_, err = client.Get(source.URL)
	if err == nil || !strings.Contains(err.Error(), "unapproved origin") {
		t.Fatalf("Get() = %v", err)
	}
	if reached.Load() {
		t.Fatal("redirect target received authenticated request")
	}
}

func TestAuthenticatedSessionCannotAdvertiseHTTPService(t *testing.T) {
	transport, err := newAuthTransport(http.DefaultTransport, config.AccountConfig{Auth: "bearer", ResolvedToken: "secret"}, "https://mail.example/session", context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.allowURLs("http://mail.example/api"); err == nil || !strings.Contains(err.Error(), "HTTPS downgrade") {
		t.Fatalf("allowURLs() = %v", err)
	}
}

func TestMailBackupAndImport(t *testing.T) {
	const raw = "From: sender@example.test\r\nTo: user@example.test\r\nSubject: Test\r\n\r\nBody\r\n"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/session":
			json.NewEncoder(writer).Encode(map[string]any{"apiUrl": server.URL + "/api", "downloadUrl": server.URL + "/download/{accountId}/{blobId}/{name}?type={type}", "uploadUrl": server.URL + "/upload/{accountId}", "capabilities": map[string]any{string(mail.URI): map[string]any{}, "urn:ietf:params:jmap:core": map[string]any{}}, "primaryAccounts": map[string]string{string(mail.URI): "a1"}, "accounts": map[string]any{"a1": map[string]any{}}})
		case request.Method == http.MethodPost && request.URL.Path == "/api":
			var envelope struct {
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}
			if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
				t.Error(err)
				return
			}
			var method string
			_ = json.Unmarshal(envelope.MethodCalls[0][0], &method)
			var result any
			switch method {
			case "Mailbox/get":
				result = map[string]any{"accountId": "a1", "state": "m1", "list": []any{map[string]any{"id": "inbox", "name": "Inbox"}}}
			case "Email/query":
				total := 1
				result = map[string]any{"accountId": "a1", "queryState": "q1", "position": 0, "ids": []string{"e1"}, "total": total}
			case "Email/get":
				result = map[string]any{"accountId": "a1", "state": "e-state", "list": []any{map[string]any{"id": "e1", "blobId": "b1", "subject": "Test", "receivedAt": "2024-01-02T03:04:05Z", "keywords": map[string]bool{"$seen": true}, "mailboxIds": map[string]bool{"inbox": true}}}, "notFound": []string{}}
			case "Email/changes":
				result = map[string]any{"accountId": "a1", "oldState": "e-state", "newState": "e-state-2", "hasMoreChanges": false, "created": []string{}, "updated": []string{}, "destroyed": []string{}}
			case "Email/import":
				result = map[string]any{"accountId": "a1", "created": map[string]any{}}
				var arguments map[string]any
				_ = json.Unmarshal(envelope.MethodCalls[0][1], &arguments)
				for creationID := range arguments["emails"].(map[string]any) {
					result.(map[string]any)["created"] = map[string]any{creationID: map[string]any{"id": "imported"}}
				}
			default:
				t.Errorf("unexpected JMAP method %s", method)
				writer.WriteHeader(500)
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"methodResponses": []any{[]any{method, result, "c1"}}})
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/download/a1/b1/"):
			writer.Header().Set("Content-Type", "message/rfc822")
			io.WriteString(writer, raw)
		case request.Method == http.MethodPost && request.URL.Path == "/upload/a1":
			if request.Header.Get("Content-Type") != "message/rfc822" {
				t.Errorf("upload Content-Type = %q", request.Header.Get("Content-Type"))
			}
			data, _ := io.ReadAll(request.Body)
			if string(data) != raw {
				t.Errorf("upload = %q", data)
			}
			json.NewEncoder(writer).Encode(map[string]any{"accountId": "a1", "blobId": "uploaded", "type": "message/rfc822", "size": len(raw)})
		default:
			http.Error(writer, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(context.Background(), config.AccountConfig{Protocol: "jmap", AllowInsecure: true, URL: server.URL + "/session", Auth: "bearer", ResolvedToken: "token", Timeout: config.Duration{Duration: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	collection, err := client.Collection(context.Background())
	if err != nil || collection.RemoteID != "a1" {
		t.Fatalf("Collection() = %#v, %v", collection, err)
	}
	objects, state, err := client.Objects(context.Background(), "")
	if err != nil || len(objects) != 1 || state != "e-state" || len(objects[0].Flags) != 1 || objects[0].Flags[0] != "$seen" || len(objects[0].MailboxIDs) != 1 || objects[0].ReceivedAt == nil {
		t.Fatalf("Objects() = %#v, %q, %v", objects, state, err)
	}
	changes, state, err := client.Objects(context.Background(), state)
	if err != nil || len(changes) != 0 || state != "e-state-2" {
		t.Fatalf("incremental Objects() = %#v, %q, %v", changes, state, err)
	}
	body, err := client.Get(context.Background(), objects[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if string(data) != raw {
		t.Fatalf("Get() = %q", data)
	}
	id, err := client.Import(context.Background(), "Inbox", []string{"$seen"}, nil, strings.NewReader(raw))
	if err != nil || id != "imported" {
		t.Fatalf("Import() = %q, %v", id, err)
	}
}
