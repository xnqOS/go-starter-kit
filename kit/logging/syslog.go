package logging

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"time"
)

var facilities = map[string]int{
	"user": 1, "daemon": 3,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// A fresh JSON handler per record avoids sharing a mutable encoding buffer.
// These operations preserve the order-sensitive WithAttrs/WithGroup semantics.
type handlerOperation struct {
	attrs []slog.Attr
	group string
}

type syslogHandler struct {
	client     *syslogClient
	options    slog.HandlerOptions
	operations []handlerOperation
}

func (h *syslogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.options.Level.Level()
}

func (h *syslogHandler) Handle(ctx context.Context, record slog.Record) error {
	var body bytes.Buffer
	var handler slog.Handler = slog.NewJSONHandler(&body, &h.options)
	for _, operation := range h.operations {
		if operation.group != "" {
			handler = handler.WithGroup(operation.group)
		} else {
			handler = handler.WithAttrs(operation.attrs)
		}
	}
	if err := handler.Handle(ctx, record); err != nil {
		return err
	}
	message := encodeSyslog(h.client.config, h.client.pid, record, bytes.TrimSuffix(body.Bytes(), []byte("\n")))
	return h.client.Send(message)
}

func (h *syslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.operations = append(slices.Clone(h.operations), handlerOperation{attrs: slices.Clone(attrs)})
	return &clone
}

func (h *syslogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.operations = append(slices.Clone(h.operations), handlerOperation{group: name})
	return &clone
}

// encodeSyslog produces RFC 5424 HEADER + NILVALUE structured data + JSON MSG.
// RFC 5424 permits at most six fractional second digits, unlike RFC3339Nano.
func encodeSyslog(cfg Config, pid int, record slog.Record, body []byte) []byte {
	severity := 6 // INFO
	switch {
	case record.Level >= slog.LevelError:
		severity = 3 // ERR
	case record.Level >= slog.LevelWarn:
		severity = 4 // WARNING
	case record.Level < slog.LevelInfo:
		severity = 7 // DEBUG
	}
	timestamp := "-"
	if !record.Time.IsZero() {
		timestamp = record.Time.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.999999Z07:00")
	}
	header := fmt.Sprintf("<%d>1 %s %s %s %d - - ", facilities[cfg.Syslog.Facility]*8+severity, timestamp, cfg.Instance, cfg.Service, pid)
	return append([]byte(header), body...)
}

type syslogClient struct {
	config Config
	pid    int
	gate   chan struct{}
	conn   net.Conn
	closed bool
	dial   func(context.Context) (net.Conn, error)
	tls    *tls.Config
}

func newSyslogClient(cfg Config) (*syslogClient, error) {
	dialer := &net.Dialer{}
	client := &syslogClient{config: cfg, pid: os.Getpid(), gate: make(chan struct{}, 1)}
	if cfg.Syslog.Network == "tls" {
		serverName := cfg.Syslog.TLSServerName
		if serverName == "" {
			serverName, _, _ = net.SplitHostPort(cfg.Syslog.Address)
		}
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
		if cfg.Syslog.TLSCAFile != "" {
			pem, err := os.ReadFile(cfg.Syslog.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("read LOG_SYSLOG_TLS_CA_FILE: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("LOG_SYSLOG_TLS_CA_FILE contains no valid CA certificates")
			}
			tlsConfig.RootCAs = roots
		}
		client.tls = tlsConfig
	}
	client.dial = func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", cfg.Syslog.Address)
	}
	return client, nil
}

// Send uses RFC 6587 octet-counting, also required by RFC 5425 over TLS.
// The timeout includes waiting for another write, connecting, and sending.
// There is no same-record retry: a partial TCP write may already be delivered.
func (c *syslogClient) Send(message []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.config.Syslog.Timeout)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return fmt.Errorf("syslog writer busy: %w", ctx.Err())
	}
	if c.closed {
		return fmt.Errorf("syslog writer is closed")
	}
	if c.conn == nil {
		conn, err := c.dial(ctx)
		if err != nil {
			return fmt.Errorf("syslog connect: %w", err)
		}
		if c.tls != nil {
			secure := tls.Client(conn, c.tls)
			if err := secure.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return fmt.Errorf("syslog TLS handshake: %w", err)
			}
			conn = secure
		}
		c.conn = conn
	}
	deadline, _ := ctx.Deadline()
	err := c.conn.SetWriteDeadline(deadline)
	if err == nil {
		frame := append(fmt.Appendf(nil, "%d ", len(message)), message...)
		err = writeAll(c.conn, frame)
	}
	if err != nil {
		_ = c.conn.Close()
		c.conn = nil
		return fmt.Errorf("syslog send: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, body []byte) error {
	for len(body) > 0 {
		n, err := writer.Write(body)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		body = body[n:]
	}
	return nil
}

func (c *syslogClient) Close() error {
	c.gate <- struct{}{}
	defer func() { <-c.gate }()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}
