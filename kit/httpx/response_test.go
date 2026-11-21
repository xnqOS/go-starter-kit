package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
	"github.com/xnqOS/go-starter-kit/kit/logging"
)

func render(t *testing.T, handler gin.HandlerFunc) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	router := gin.New()
	router.GET("/", handler)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "untrusted-client-id")
	router.ServeHTTP(w, req)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected JSON response: %#v", w.Header())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code", "message", "data", "request_id"} {
		if _, ok := body[name]; !ok {
			t.Fatalf("missing %s: %s", name, w.Body.String())
		}
	}
	var id string
	if err := json.Unmarshal(body["request_id"], &id); err != nil || id == "" || id != w.Header().Get("X-Request-ID") || id == "untrusted-client-id" {
		t.Fatalf("invalid request correlation: %s %v", w.Body.String(), err)
	}
	return w, body
}

func TestSuccessAndNilData(t *testing.T) {
	for _, value := range []any{nil, map[string]any{"id": "one", "enabled": false, "count": 0}} {
		w, body := render(t, func(c *gin.Context) {
			first := RequestID(c)
			if RequestID(c) != first {
				t.Fatal("request ID changed")
			}
			Data(c, http.StatusCreated, value)
		})
		if w.Code != http.StatusCreated || string(body["code"]) != `"ok"` || string(body["message"]) != `"success"` || len(body) != 4 {
			t.Fatalf("unexpected success: %s", w.Body.String())
		}
		expected, err := json.Marshal(value)
		if err != nil || string(body["data"]) != string(expected) {
			t.Fatalf("data changed: %s", w.Body.String())
		}
	}
}

func TestNilPageRemainsAnArrayWithZeroTotal(t *testing.T) {
	w, body := render(t, func(c *gin.Context) {
		Page(c, []string(nil), Pagination{Limit: 20})
	})
	if w.Code != http.StatusOK || string(body["data"]) != "[]" || string(body["pagination"]) != `{"limit":20,"offset":0,"total":0}` {
		t.Fatalf("unexpected empty page: %s", w.Body.String())
	}
}

func TestErrorsUseHTTPStatusAndPublicMessages(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		status        int
		code, message string
	}{
		{"invalid", apperror.ErrInvalid, 400, "invalid_input", "invalid input"},
		{"not found", fmt.Errorf("lookup failed: %w", apperror.ErrNotFound), 404, "not_found", "resource not found"},
		{"unauthorized", apperror.ErrUnauthorized, 401, "unauthorized", "authentication required"},
		{"forbidden", apperror.ErrForbidden, 403, "forbidden", "access denied"},
		{"conflict", apperror.ErrConflict, 409, "conflict", "resource conflict"},
		{"rate limit", apperror.ErrRateLimited, 429, "rate_limited", "too many requests"},
		{"unavailable", apperror.ErrUnavailable, 503, "unavailable", "service unavailable"},
		{"canceled", fmt.Errorf("wrapped: %w", context.Canceled), 499, "request_canceled", "request canceled"},
		{"deadline", context.DeadlineExceeded, 503, "unavailable", "request deadline exceeded"},
		{"cancellation takes precedence over public errors", errors.Join(context.Canceled, apperror.New(apperror.ErrConflict, "private_conflict", "resource changed")), 499, "request_canceled", "request canceled"},
		{"deadline takes precedence over public errors", errors.Join(context.DeadlineExceeded, apperror.New(apperror.ErrInvalid, "private_validation", "resource invalid")), 503, "unavailable", "request deadline exceeded"},
		{"public business error", fmt.Errorf("secret diagnostic: %w", apperror.New(apperror.ErrConflict, "order_already_paid", "order is already paid")), 409, "order_already_paid", "order is already paid"},
		{"invalid helper uses default code", apperror.Invalid("name is required"), 400, "invalid_input", "name is required"},
		{"public error of another kind is ignored", fmt.Errorf("%w: %w", apperror.ErrNotFound, apperror.New(apperror.ErrInvalid, "bad_sku", "sku malformed")), 404, "not_found", "resource not found"},
		{"public error matching the status is used", errors.Join(apperror.New(apperror.ErrInvalid, "bad_sku", "sku malformed"), apperror.New(apperror.ErrNotFound, "sku_missing", "sku not found")), 404, "sku_missing", "sku not found"},
		{"custom As public error", customPublicError{kind: apperror.ErrConflict, public: apperror.New(apperror.ErrConflict, "order_already_paid", "order is already paid")}, 409, "order_already_paid", "order is already paid"},
		{"custom As must match selected kind", customPublicError{kind: apperror.ErrNotFound, public: apperror.New(apperror.ErrInvalid, "bad_sku", "sku malformed")}, 404, "not_found", "resource not found"},
		{"walk continues after mismatched custom As", errors.Join(customPublicError{kind: apperror.ErrNotFound, public: apperror.New(apperror.ErrInvalid, "bad_sku", "sku malformed")}, apperror.New(apperror.ErrNotFound, "sku_missing", "sku not found")), 404, "sku_missing", "sku not found"},
		{"custom As returning false is ignored", customPublicError{kind: apperror.ErrConflict, public: apperror.New(apperror.ErrConflict, "private_code", "private message"), reject: true}, 409, "conflict", "resource conflict"},
		{"wrapped validation details stay private", fmt.Errorf("secret diagnostic: %w", apperror.ErrInvalid), 400, "invalid_input", "invalid input"},
		{"unknown error", errors.New("secret diagnostic"), 500, "internal_error", "an internal error occurred"},
		{"unclassified public error", apperror.New(errors.New("unknown kind"), "private_code", "secret diagnostic"), 500, "internal_error", "an internal error occurred"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, body := render(t, func(c *gin.Context) { Fail(c, test.err) })
			if w.Code != test.status || string(body["code"]) != strconvJSON(test.code) || string(body["message"]) != strconvJSON(test.message) || string(body["data"]) != "null" || len(body) != 4 {
				t.Fatalf("unexpected error: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("diagnostic leaked: %s", w.Body.String())
			}
		})
	}
}

// Exposes a public error without an Unwrap method, as supported by errors.As.
type customPublicError struct {
	kind   error
	public *apperror.Error
	reject bool
}

func (e customPublicError) Error() string        { return "internal diagnostic" }
func (e customPublicError) Is(target error) bool { return errors.Is(e.kind, target) }
func (e customPublicError) As(target any) bool {
	if public, ok := target.(**apperror.Error); ok {
		*public = e.public
		return !e.reject
	}
	return false
}

func TestContextFailuresDoNotEmitErrorLogs(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		var logs bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		w, _ := render(t, func(c *gin.Context) {
			c.Request = c.Request.WithContext(logging.WithLogger(c.Request.Context(), logger))
			Fail(c, fmt.Errorf("database operation: %w", err))
		})
		if logs.Len() != 0 {
			t.Fatalf("ordinary context failure logged as error: %s", logs.String())
		}
		if err == context.DeadlineExceeded && w.Header().Get("Retry-After") != "1" {
			t.Fatal("deadline response has no retry hint")
		}
	}
}

func strconvJSON(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func TestRejectAbortsAndNoContentHasNoEnvelope(t *testing.T) {
	router := gin.New()
	router.GET("/reject", func(c *gin.Context) { Reject(c, 403, "forbidden", "access denied") }, func(c *gin.Context) { t.Error("handler ran after rejection") })
	router.DELETE("/empty", NoContent)
	router.DELETE("/data", func(c *gin.Context) { Data(c, http.StatusNoContent, map[string]any{"must_not_appear": true}) })
	for _, path := range []string{"/empty", "/data"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("DELETE", path, nil))
		if w.Code != 204 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" || w.Header().Get("X-Request-ID") == "" {
			t.Fatalf("invalid 204 response: %d %#v %s", w.Code, w.Header(), w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/reject", nil))
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestRequestIDHeaderSetOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	id := RequestID(c)
	w.Header().Del("X-Request-ID")
	if RequestID(c) != id || w.Header().Get("X-Request-ID") != "" {
		t.Fatal("RequestID should only set the header when it creates the ID")
	}
}
