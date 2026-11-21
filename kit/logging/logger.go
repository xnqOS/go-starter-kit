package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
)

// New returns a standard logger and its resource cleanup function. Connections
// are opened on the first remote write, so an offline log server doesn't
// prevent the API from starting. Config and certificate errors fail early.
func New(cfg Config) (*slog.Logger, func() error, error) {
	return newLogger(cfg, os.Stdout, os.Stderr)
}

func newLogger(cfg Config, stdout, diagnostics io.Writer) (*slog.Logger, func() error, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	opts := slog.HandlerOptions{Level: cfg.Level}
	handlers := make([]slog.Handler, 0, len(cfg.Outputs))
	closeOutput := func() error { return nil }
	for _, output := range cfg.Outputs {
		switch output {
		case "stdout":
			handlers = append(handlers, slog.NewJSONHandler(stdout, &opts))
		case "syslog":
			client, err := newSyslogClient(cfg)
			if err != nil {
				return nil, nil, err
			}
			handlers = append(handlers, &syslogHandler{client: client, options: opts})
			closeOutput = client.Close
		}
	}
	handler := &fanoutHandler{
		handlers: handlers, diagnostics: diagnostics, diagnosticMu: new(sync.Mutex),
	}
	logger := slog.New(handler).With("service", cfg.Service, "env", cfg.Env, "instance", cfg.Instance)
	return logger, closeOutput, nil
}

// fanoutHandler keeps Go 1.25 compatibility. A failed output must not prevent
// the others from receiving a record. slog ignores returned Handler errors,
// so diagnostics go to a separate writer without recursively using the logger.
type fanoutHandler struct {
	handlers     []slog.Handler
	diagnostics  io.Writer
	diagnosticMu *sync.Mutex
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var failures []error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, record.Level) {
			if err := handler.Handle(ctx, record.Clone()); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := errors.Join(failures...); err != nil {
		h.diagnosticMu.Lock()
		_, _ = fmt.Fprintf(h.diagnostics, "logging output failed: %v\n", err)
		h.diagnosticMu.Unlock()
		return err
	}
	return nil
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.handlers = make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		clone.handlers[i] = handler.WithAttrs(slices.Clone(attrs))
	}
	return &clone
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.handlers = make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		clone.handlers[i] = handler.WithGroup(name)
	}
	return &clone
}
