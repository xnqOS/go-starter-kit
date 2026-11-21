package logging

import (
	"context"
	"log/slog"
)

type contextKey struct{}

// WithLogger adds an immutable request logger to the standard request context.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, logger)
}

// FromContext returns the request logger, or the standard default logger for
// callers such as jobs and command-line tools without a request context.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}
