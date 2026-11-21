package example

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/httpx"
)

func request(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestHTTPCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/items"), NewService(NewMemoryRepo()))
	created := request(router, "POST", "/items", `{"name":"hello"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var response struct {
		Data Example `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	path := "/items/" + response.Data.ID
	if created.Header().Get("Location") != path {
		t.Fatal("missing Location header")
	}
	assertEnvelope(t, created, "ok")
	for _, test := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", path, "", 200, "ok"},
		{"GET", "/items?limit=1&offset=0", "", 200, "ok"},
		{"PUT", path, `{"name":"updated"}`, 200, "ok"},
		{"DELETE", path, "", 204, ""},
		{"GET", path, "", 404, "not_found"},
		{"PUT", path, `{"name":"updated"}`, 404, "not_found"},
		{"DELETE", path, "", 404, "not_found"},
		{"POST", "/items", `{"name":" "}`, 400, "invalid_input"},
		{"POST", "/items", `{"unexpected":true}`, 400, "invalid_json"},
		{"POST", "/items", `{"name":"one"} {"name":"two"}`, 400, "invalid_json"},
		{"POST", "/items", `null`, 400, "invalid_input"},
		{"POST", "/items", `{`, 400, "invalid_json"},
		{"GET", "/items?limit=101", "", 400, "invalid_input"},
		{"GET", "/items?offset=-1", "", 400, "invalid_input"},
		{"GET", "/items?limit=invalid", "", 400, "invalid_input"},
	} {
		w := request(router, test.method, test.path, test.body)
		if w.Code != test.status {
			t.Errorf("%s %s: got %d, want %d: %s", test.method, test.path, w.Code, test.status, w.Body.String())
		}
		if test.status == http.StatusNoContent {
			if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" || w.Header().Get("X-Request-ID") == "" {
				t.Fatalf("204 must have no body and retain request ID: %#v %s", w.Header(), w.Body.String())
			}
		} else {
			assertEnvelope(t, w, test.code)
		}
	}
	oversized := request(router, "POST", "/items", `{"name":"`+strings.Repeat("x", 1<<20)+`"}`)
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", oversized.Code)
	}
	assertEnvelope(t, oversized, "body_too_large")
	badType := httptest.NewRequest("POST", "/items", strings.NewReader(`{"name":"valid"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, badType)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", w.Code)
	}
	assertEnvelope(t, w, "unsupported_media_type")
}

func assertEnvelope(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	var envelope struct {
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != code || envelope.Message == "" || len(envelope.Data) == 0 || envelope.RequestID == "" || envelope.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("invalid envelope: %s", w.Body.String())
	}
	if w.Code >= 400 && string(envelope.Data) != "null" {
		t.Fatalf("error data must be null: %s", w.Body.String())
	}
}

func TestHTTPPaginationMetadata(t *testing.T) {
	router := gin.New()
	RegisterRoutes(router.Group("/items"), NewService(NewMemoryRepo()))
	check := func(path string, length int, pagination httpx.Pagination) {
		t.Helper()
		w := request(router, "GET", path, "")
		assertEnvelope(t, w, "ok")
		var page struct {
			Data       []Example        `json:"data"`
			Pagination httpx.Pagination `json:"pagination"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || page.Data == nil || len(page.Data) != length || page.Pagination != pagination {
			t.Fatalf("unexpected page: %s", w.Body.String())
		}
	}
	check("/items", 0, httpx.Pagination{Limit: 20})
	for _, name := range []string{"one", "two", "three"} {
		w := request(router, "POST", "/items", `{"name":"`+name+`"}`)
		if w.Code != 201 {
			t.Fatal(w.Body.String())
		}
	}
	check("/items?limit=1&offset=1", 1, httpx.Pagination{Limit: 1, Offset: 1, Total: 3})
	check("/items?limit=0", 3, httpx.Pagination{Limit: 20, Total: 3})
	check("/items?offset=99", 0, httpx.Pagination{Limit: 20, Offset: 99, Total: 3})
}

func TestHTTPStorageFailureDoesNotExposeDetails(t *testing.T) {
	router := gin.New()
	RegisterRoutes(router.Group("/items"), NewService(failingRepo{}))
	w := request(router, "POST", "/items", `{"name":"valid"}`)
	if w.Code != 500 || strings.Contains(w.Body.String(), "storage unavailable") {
		t.Fatalf("unexpected error response: %d %s", w.Code, w.Body.String())
	}
}
