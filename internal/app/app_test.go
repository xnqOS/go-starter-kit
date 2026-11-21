package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/httpx"
	"starter-kit/internal/config"
)

func TestMemoryApplicationRoutesAndRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	application, err := New(context.Background(), config.Config{HTTPAddr: ":0", Storage: "memory"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	application.Handler.GET("/panic", func(c *gin.Context) { panic("test panic") })
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/health", 200}, {"GET", "/ready", 200},
		{"GET", "/api/v1/example", 200}, {"GET", "/missing", 404},
		{"PATCH", "/api/v1/example", 405}, {"GET", "/panic", 500},
	} {
		w := httptest.NewRecorder()
		application.Handler.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status {
			t.Errorf("%s %s: %d %s", test.method, test.path, w.Code, w.Body.String())
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Error("missing request ID")
		}
		if strings.Contains(w.Body.String(), "test panic") {
			t.Error("panic details leaked")
		}
		if strings.HasPrefix(test.path, "/api/") || test.status >= 400 {
			var envelope httpx.Response
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code == "" || envelope.Message == "" || envelope.RequestID != w.Header().Get("X-Request-ID") {
				t.Fatalf("response envelope missing: %s %v", w.Body.String(), err)
			}
		}
	}
}

func TestBusinessAndErrorLogsShareHTTPResponseRequestID(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	application, err := New(context.Background(), config.Config{HTTPAddr: ":0", Storage: "memory"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	application.Handler.GET("/failure", func(c *gin.Context) {
		httpx.Fail(c, errors.New("database test failure"))
	})
	for _, test := range []struct {
		method, path, body string
		status             int
		level              string
	}{
		{"POST", "/api/v1/example", `{"name":"logged"}`, 201, "INFO"},
		{"GET", "/failure", "", 500, "ERROR"},
	} {
		output.Reset()
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		application.Handler.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Fatal(response.Code)
		}
		var envelope httpx.Response
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.RequestID != response.Header().Get("X-Request-ID") {
			t.Fatalf("response request ID missing: %s %v", response.Body.String(), err)
		}
		decoder := json.NewDecoder(&output)
		count := 0
		for decoder.More() {
			var record map[string]any
			if err := decoder.Decode(&record); err != nil {
				t.Fatal(err)
			}
			if record["request_id"] != response.Header().Get("X-Request-ID") || record["level"] != test.level {
				t.Fatalf("request correlation or level lost: %#v", record)
			}
			count++
		}
		if count != 2 {
			t.Fatalf("expected business/error log and access log, got %d", count)
		}
	}
}

func TestInvalidStorageFailsBeforeStarting(t *testing.T) {
	if _, err := New(context.Background(), config.Config{HTTPAddr: ":0", Storage: "unknown"}, nil); err == nil {
		t.Fatal("invalid storage accepted")
	}
	if _, err := New(context.Background(), config.Config{HTTPAddr: ":0", Storage: "postgres"}, nil); err == nil {
		t.Fatal("missing database URL accepted")
	}
}
func TestClientIPOnlyTrustsConfiguredIngress(t *testing.T) {
	for _, test := range []struct {
		name    string
		trusted []string
		want    string
	}{{"untrusted", nil, "10.1.2.3"}, {"configured", []string{"10.1.0.0/16"}, "198.51.100.7"}, {"different_network", []string{"192.0.2.0/24"}, "10.1.2.3"}} {
		t.Run(test.name, func(t *testing.T) {
			application, err := New(context.Background(), config.Config{HTTPAddr: ":0", Storage: "memory", TrustedProxies: test.trusted}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			application.Handler.GET("/client", func(c *gin.Context) { c.String(200, c.ClientIP()) })
			req := httptest.NewRequest("GET", "/client", nil)
			req.RemoteAddr = "10.1.2.3:1234"
			req.Header.Set("X-Forwarded-For", "198.51.100.7")
			w := httptest.NewRecorder()
			application.Handler.ServeHTTP(w, req)
			if w.Body.String() != test.want {
				t.Fatalf("client IP = %q, expected %q", w.Body.String(), test.want)
			}
		})
	}
}
