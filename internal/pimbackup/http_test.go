package pimbackup

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/pimbackup/config"
)

func TestHTTPRejectsNonLoopbackHostWithoutAuthentication(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	service, err := OpenService(context.Background(), cfg, ServiceOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Host = "attacker.example"
	response := httptest.NewRecorder()
	service.HTTPHandler(buildinfo.Info{}).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host status = %d", response.Code)
	}
}

func TestHTTPHealthAndAuthentication(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	cfg.Server.ResolvedAuthToken = "token"
	service, err := OpenService(context.Background(), cfg, ServiceOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	handler := service.HTTPHandler(buildinfo.Info{Version: "test"})

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	request.Header.Set("Authorization", "Bearer token")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d: %s", authorized.Code, authorized.Body.String())
	}
	var response buildinfo.Info
	if err := json.NewDecoder(authorized.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Version != "test" {
		t.Fatalf("version = %q", response.Version)
	}

	post := httptest.NewRequest(http.MethodPost, "/api/v1/backup", strings.NewReader(`{}`))
	post.Header.Set("Authorization", "Bearer token")
	withoutJSONType := httptest.NewRecorder()
	handler.ServeHTTP(withoutJSONType, post)
	if withoutJSONType.Code != http.StatusBadRequest {
		t.Fatalf("POST without JSON Content-Type status = %d", withoutJSONType.Code)
	}
}
