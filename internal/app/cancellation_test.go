package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xnqOS/go-starter-kit/kit/httpx"
)

func TestCanceledRequestsHaveQuietAccessLogs(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		var logs bytes.Buffer
		router := gin.New()
		router.Use(middleware(slog.New(slog.NewJSONHandler(&logs, nil))))
		router.GET("/", func(c *gin.Context) { httpx.Fail(c, c.Request.Context().Err()) })
		var requestCtx context.Context
		var cancel context.CancelFunc
		want := 499
		if deadline {
			requestCtx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = 503
		} else {
			requestCtx, cancel = context.WithCancel(context.Background())
			cancel()
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest("GET", "/", nil).WithContext(requestCtx))
		cancel()
		if out.Code != want || strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"level":"INFO"`) {
			t.Fatalf("status %d, logs %s", out.Code, logs.String())
		}
	}
}
