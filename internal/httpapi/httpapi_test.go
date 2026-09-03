package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONIsStrictAndBounded(t *testing.T) {
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"ok","extra":true}`))
	request.Header.Set("Content-Type", "application/json")
	var target struct {
		Name string `json:"name"`
	}
	if err := DecodeJSON(httptest.NewRecorder(), request, &target); err == nil {
		t.Fatal("DecodeJSON accepted an unknown field")
	}
}

func TestDecodeOptionalJSONAcceptsEmptyBody(t *testing.T) {
	request := httptest.NewRequest("POST", "/", strings.NewReader(""))
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = -1
	if err := DecodeOptionalJSON(httptest.NewRecorder(), request, new(struct{})); err != nil {
		t.Fatalf("DecodeOptionalJSON empty body: %v", err)
	}
}

func TestPagination(t *testing.T) {
	request := httptest.NewRequest("GET", "/?limit=25&offset=5", nil)
	limit, offset, err := Pagination(request)
	if err != nil || limit != 25 || offset != 5 {
		t.Fatalf("Pagination = %d, %d, %v", limit, offset, err)
	}
	request = httptest.NewRequest("GET", "/?limit=1001", nil)
	if _, _, err := Pagination(request); err == nil {
		t.Fatal("Pagination accepted an excessive limit")
	}
}

func TestWriteJSON(t *testing.T) {
	response := httptest.NewRecorder()
	WriteJSON(response, 201, map[string]string{"status": "ok"})
	if response.Code != 201 || response.Header().Get("Cache-Control") != "no-store" || response.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("response = %#v, %q", response.Result().Header, response.Body.String())
	}
}
