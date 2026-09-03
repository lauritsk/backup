package appbackup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lauritsk/backup/internal/buildinfo"
)

func TestHTTPAuthentication(t *testing.T) {
	cfg := serviceConfig(t.TempDir(), t.TempDir())
	cfg.Server.ResolvedAuthToken = "token"
	service, err := OpenService(context.Background(), cfg, ServiceOptions{Restic: &fakeRestic{}, Databases: fakeDatabases{}, Hooks: fakeHooks{events: new([]string)}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	handler := service.HTTPHandler(buildinfo.Info{})
	request := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/applications", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/applications", nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
