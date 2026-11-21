package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/httpx"
	"github.com/xnqOS/go-starter-kit/kit/logging"
)

func middleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		requestID := httpx.RequestID(c)
		requestLogger := logger.With("request_id", requestID)
		c.Request = c.Request.WithContext(logging.WithLogger(c.Request.Context(), requestLogger))
		defer func() {
			if recovered := recover(); recovered != nil {
				requestLogger.ErrorContext(c.Request.Context(), "request panic", "panic", recovered)
				if !c.Writer.Written() {
					httpx.Reject(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
				} else {
					c.Abort()
				}
			}
			level := slog.LevelInfo
			if c.Writer.Status() == 499 || (c.Writer.Status() == http.StatusServiceUnavailable && errors.Is(c.Request.Context().Err(), context.DeadlineExceeded)) {
				level = slog.LevelInfo
			} else if c.Writer.Status() >= http.StatusInternalServerError {
				level = slog.LevelError
			} else if c.Writer.Status() >= http.StatusBadRequest {
				level = slog.LevelWarn
			}
			requestLogger.Log(c.Request.Context(), level, "HTTP request",
				"method", c.Request.Method,
				"route", c.FullPath(), "status", c.Writer.Status(), "duration", time.Since(start))
		}()
		c.Next()
	}
}
