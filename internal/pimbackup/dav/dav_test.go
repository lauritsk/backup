package dav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/pimbackup/config"
)

func TestDiscoverEndpointFallsBackToWellKnown(t *testing.T) {
	original := discoverCardDAV
	discoverCardDAV = func(context.Context, string) (string, error) { return "", io.EOF }
	defer func() { discoverCardDAV = original }()
	got, err := discoverEndpoint(context.Background(), config.AccountConfig{Protocol: "carddav", Host: "dav.example.test"})
	if err != nil || got != "https://dav.example.test/.well-known/carddav" {
		t.Fatalf("discoverEndpoint() = %q, %v", got, err)
	}
}

func TestCardDAVDiscoveryAndRoundTrip(t *testing.T) {
	const card = "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ada\r\nEND:VCARD\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "user" || password != "secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(request.Body)
		switch {
		case request.Method == "PROPFIND" && strings.Contains(string(body), "current-user-principal"):
			multiStatus(writer, `<d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && strings.Contains(string(body), "addressbook-home-set"):
			multiStatus(writer, `<d:response><d:href>/principal/</d:href><d:propstat><d:prop><card:addressbook-home-set><d:href>/dav/</d:href></card:addressbook-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && strings.Contains(string(body), "addressbook-description"):
			multiStatus(writer, `<d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:displayname>People</d:displayname><d:resourcetype><d:collection/><card:addressbook/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && request.URL.Path == "/dav/":
			multiStatus(writer, `<d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype><d:getlastmodified>Tue, 02 Jan 2024 03:04:05 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/dav/ada.vcf</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>53</d:getcontentlength><d:getcontenttype>text/vcard</d:getcontenttype><d:getetag>"etag-1"</d:getetag><d:getlastmodified>Tue, 02 Jan 2024 03:04:05 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == http.MethodGet && request.URL.Path == "/dav/ada.vcf":
			writer.Header().Set("Content-Type", "text/vcard")
			io.WriteString(writer, card)
		case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/dav/pimbackup-"):
			if request.Header.Get("Content-Type") != "text/vcard" {
				t.Errorf("PUT Content-Type = %q", request.Header.Get("Content-Type"))
			}
			if request.ContentLength != int64(len(card)) {
				t.Errorf("PUT Content-Length = %d", request.ContentLength)
			}
			if string(body) != card {
				t.Errorf("PUT body = %q", body)
			}
			writer.Header().Set("Location", "stored.vcf")
			writer.WriteHeader(http.StatusCreated)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(context.Background(), config.AccountConfig{Protocol: "carddav", AllowInsecure: true, URL: server.URL + "/dav/", Auth: "basic", Username: "user", ResolvedPassword: "secret", Timeout: config.Duration{Duration: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	collections, err := client.Collections(context.Background())
	if err != nil || len(collections) != 1 || collections[0].Name != "People" {
		t.Fatalf("Collections() = %#v, %v", collections, err)
	}
	client.account.URL = ""
	discovered, err := client.Collections(context.Background())
	if err != nil || len(discovered) != 1 || discovered[0].RemoteID != "/dav/" {
		t.Fatalf("autodiscovered Collections() = %#v, %v", discovered, err)
	}
	objects, _, err := client.Objects(context.Background(), collections[0])
	if err != nil || len(objects) != 1 || objects[0].ETag != "etag-1" {
		t.Fatalf("Objects() = %#v, %v", objects, err)
	}
	body, contentType, err := client.Get(context.Background(), objects[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if string(data) != card || contentType != "text/vcard" {
		t.Fatalf("Get() = %q, %q", data, contentType)
	}
	remoteID, err := client.Put(context.Background(), collections[0].URL, "contact", "text/vcard", strings.NewReader(card))
	if err != nil {
		t.Fatal(err)
	}
	if remoteID != server.URL+"/dav/stored.vcf" {
		t.Fatalf("Put() remote ID = %q", remoteID)
	}
}

func TestGetRefusesCredentialsForAnotherOrigin(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/contact.vcf", http.StatusFound)
	}))
	defer source.Close()
	client, err := New(context.Background(), config.AccountConfig{Protocol: "carddav", AllowInsecure: true, URL: source.URL + "/dav/", Auth: "basic", Username: "user", ResolvedPassword: "secret", Timeout: config.Duration{Duration: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _, err = client.Objects(context.Background(), Collection{RemoteID: "/", URL: target.URL + "/", SyncToken: "token"})
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("Objects() = %v", err)
	}
	_, _, err = client.Get(context.Background(), Object{RemoteID: "/contact.vcf"})
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("Get() = %v", err)
	}
	if reached.Load() {
		t.Fatal("unapproved origin received request")
	}
}

func TestSyncCollectionUsesAndAdvancesToken(t *testing.T) {
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestBody = string(body)
		if request.Method != "REPORT" {
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/xml")
		writer.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(writer, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/dav/changed.vcf</d:href><d:propstat><d:prop><d:getetag>"two"</d:getetag><d:getcontenttype>text/vcard</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/dav/deleted.vcf</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response><d:sync-token>token-2</d:sync-token></d:multistatus>`)
	}))
	defer server.Close()
	client, err := New(context.Background(), config.AccountConfig{Protocol: "carddav", AllowInsecure: true, URL: server.URL + "/dav/", Timeout: config.Duration{Duration: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	objects, token, err := client.Objects(context.Background(), Collection{RemoteID: "/dav/", URL: server.URL + "/dav/", SyncToken: "token-1"})
	if err != nil || token != "token-2" || len(objects) != 1 || objects[0].RemoteID != "/dav/changed.vcf" || objects[0].ETag != "two" {
		t.Fatalf("Objects() = %#v, %q, %v", objects, token, err)
	}
	if !strings.Contains(requestBody, "<d:sync-token>token-1</d:sync-token>") {
		t.Fatalf("sync request did not contain prior token: %s", requestBody)
	}
}

func TestCalDAVDiscoveryAndRestore(t *testing.T) {
	const event = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:one\r\nSUMMARY:Meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(request.Body)
		switch {
		case request.Method == "PROPFIND" && strings.Contains(string(body), "current-user-principal"):
			multiStatus(writer, `<d:response><d:href>/cal/</d:href><d:propstat><d:prop><d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && strings.Contains(string(body), "calendar-home-set"):
			multiStatus(writer, `<d:response><d:href>/principal/</d:href><d:propstat><d:prop><cal:calendar-home-set><d:href>/cal/</d:href></cal:calendar-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == "PROPFIND" && strings.Contains(string(body), "calendar-description"):
			multiStatus(writer, `<d:response><d:href>/cal/</d:href><d:propstat><d:prop><d:displayname>Schedule</d:displayname><d:resourcetype><d:collection/><cal:calendar/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/cal/pimbackup-"):
			if request.Header.Get("Content-Type") != "text/calendar" {
				t.Errorf("PUT Content-Type = %q", request.Header.Get("Content-Type"))
			}
			if string(body) != event {
				t.Errorf("PUT body = %q", body)
			}
			writer.WriteHeader(http.StatusCreated)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(context.Background(), config.AccountConfig{Protocol: "caldav", AllowInsecure: true, URL: server.URL + "/cal/", Auth: "bearer", ResolvedToken: "token", Timeout: config.Duration{Duration: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	collections, err := client.Collections(context.Background())
	if err != nil || len(collections) != 1 || collections[0].Name != "Schedule" || collections[0].Kind != "calendar" {
		t.Fatalf("Collections() = %#v, %v", collections, err)
	}
	if _, err := client.Put(context.Background(), collections[0].URL, "calendar", "text/calendar", strings.NewReader(event)); err != nil {
		t.Fatal(err)
	}
}

func multiStatus(writer http.ResponseWriter, responses string) {
	writer.Header().Set("Content-Type", "application/xml; charset=utf-8")
	writer.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(writer, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav" xmlns:cal="urn:ietf:params:xml:ns:caldav">`+responses+`</d:multistatus>`)
}
