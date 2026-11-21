package logging

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"
)

type captureConn struct {
	mu       sync.Mutex
	body     bytes.Buffer
	closed   bool
	maxWrite int
}

func (c *captureConn) Write(body []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.maxWrite > 0 && len(body) > c.maxWrite {
		body = body[:c.maxWrite]
	}
	return c.body.Write(body)
}
func (c *captureConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (*captureConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*captureConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*captureConn) SetDeadline(time.Time) error      { return nil }
func (*captureConn) SetReadDeadline(time.Time) error  { return nil }
func (*captureConn) SetWriteDeadline(time.Time) error { return nil }
func (c *captureConn) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.body.Bytes())
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	prefix, err := reader.ReadString(' ')
	if err != nil {
		return nil, err
	}
	length, err := strconv.Atoi(strings.TrimSuffix(prefix, " "))
	if err != nil || length < 1 || length > 1<<20 {
		return nil, errors.New("invalid octet-counting prefix")
	}
	message := make([]byte, length)
	_, err = io.ReadFull(reader, message)
	return message, err
}

func frames(t *testing.T, conn *captureConn) [][]byte {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(conn.snapshot()))
	var result [][]byte
	for {
		message, err := readFrame(reader)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, message)
	}
}

func capturedHandler(t *testing.T, cfg Config, conn *captureConn) *syslogHandler {
	t.Helper()
	client, err := newSyslogClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.dial = func(context.Context) (net.Conn, error) { return conn, nil }
	t.Cleanup(func() { _ = client.Close() })
	return &syslogHandler{client: client, options: slog.HandlerOptions{Level: cfg.Level}}
}

func TestRFC5424IdentitySeverityUnicodeAndFraming(t *testing.T) {
	conn := &captureConn{maxWrite: 7} // force partial writes
	fixed := time.Date(2026, 10, 15, 12, 30, 0, 123456789, time.UTC)
	for _, service := range []string{"order-api", "billing-api"} {
		cfg := DefaultConfig()
		cfg.Service, cfg.Instance, cfg.Level = service, "worker-1", slog.LevelDebug
		handler := capturedHandler(t, cfg, conn)
		for _, test := range []struct {
			level    slog.Level
			priority string
		}{
			{slog.LevelDebug, "<135>1"}, {slog.LevelInfo, "<134>1"},
			{slog.LevelWarn, "<132>1"}, {slog.LevelError, "<131>1"},
		} {
			record := slog.NewRecord(fixed, test.level, "创建成功\nnext line", 0)
			record.AddAttrs(slog.String("request_id", "req-123"))
			if err := handler.Handle(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			all := frames(t, conn)
			parts := strings.SplitN(string(all[len(all)-1]), " ", 8)
			if len(parts) != 8 || parts[0] != test.priority || parts[1] != "2026-10-15T12:30:00.123456Z" || parts[2] != "worker-1" || parts[3] != service || parts[5] != "-" || parts[6] != "-" {
				t.Fatalf("invalid RFC 5424 header: %q", parts)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(parts[7]), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["msg"] != record.Message || payload["request_id"] != "req-123" {
				t.Fatalf("lost fields: %#v", payload)
			}
		}
	}
	if len(frames(t, conn)) != 8 {
		t.Fatal("lost or merged frames")
	}
}

func TestSyslogHandlerStandardSlogSemantics(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Level = slog.LevelDebug
	conn := &captureConn{}
	handler := capturedHandler(t, cfg, conn)
	results := func() []map[string]any {
		var result []map[string]any
		for _, message := range frames(t, conn) {
			parts := strings.SplitN(string(message), " ", 8)
			var item map[string]any
			if err := json.Unmarshal([]byte(parts[7]), &item); err != nil {
				t.Fatal(err)
			}
			result = append(result, item)
		}
		return result
	}
	if err := slogtest.TestHandler(handler, results); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleMetadataFilteringAndRequestContext(t *testing.T) {
	var stdout, diagnostics bytes.Buffer
	cfg := DefaultConfig()
	cfg.Service, cfg.Env, cfg.Instance = "order-api", "test", "worker-2"
	logger, closeLogs, err := newLogger(cfg, &stdout, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogs()
	ctx := WithLogger(context.Background(), logger.With("request_id", "request-1"))
	FromContext(ctx).DebugContext(ctx, "filtered out")
	FromContext(ctx).InfoContext(ctx, "created", "module", "order")
	var record map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"service": "order-api", "env": "test", "instance": "worker-2", "request_id": "request-1", "module": "order", "msg": "created"} {
		if record[key] != value {
			t.Errorf("%s: got %v want %s", key, record[key], value)
		}
	}
	if diagnostics.Len() != 0 {
		t.Fatal(diagnostics.String())
	}
}

func TestFanoutKeepsLocalLogWhenRemoteFails(t *testing.T) {
	var stdout, diagnostics bytes.Buffer
	cfg := DefaultConfig()
	cfg.Outputs = []string{"syslog", "stdout"} // failed output is deliberately first
	logger, closeLogs, err := newLogger(cfg, &stdout, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogs()
	remote := logger.Handler().(*fanoutHandler).handlers[0].(*syslogHandler)
	remote.client.dial = func(context.Context) (net.Conn, error) { return nil, errors.New("offline") }
	logger.Error("important message")
	if !strings.Contains(stdout.String(), "important message") || !strings.Contains(diagnostics.String(), "syslog connect") {
		t.Fatalf("stdout=%s diagnostics=%s", &stdout, &diagnostics)
	}
}

func TestConcurrentRecordsKeepIndividualFrames(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Syslog.Timeout = 5 * time.Second
	conn := &captureConn{}
	logger := slog.New(capturedHandler(t, cfg, conn)).With("service", cfg.Service)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() { logger.WithGroup("job").Info("completed", "number", i) })
	}
	wg.Wait()
	if len(frames(t, conn)) != 50 {
		t.Fatal("concurrent messages were merged or lost")
	}
}

func TestRemoteFailureReconnectsOnNextRecord(t *testing.T) {
	cfg := DefaultConfig()
	client, err := newSyslogClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	broken, good := &captureConn{closed: true}, &captureConn{}
	calls := 0
	client.dial = func(context.Context) (net.Conn, error) {
		calls++
		if calls == 1 {
			return broken, nil
		}
		return good, nil
	}
	if err := client.Send([]byte("first")); err == nil {
		t.Fatal("accepted failed write")
	}
	if calls != 1 {
		t.Fatal("same record was retried and could be duplicated")
	}
	if err := client.Send([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || string(frames(t, good)[0]) != "second" {
		t.Fatal("next message did not reconnect")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Send([]byte("after close")); err == nil {
		t.Fatal("wrote after close")
	}
}

func TestRemoteTimeoutAndQueueWaitAreBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Syslog.Timeout = 30 * time.Millisecond
	client, err := newSyslogClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe() // no ports required; peer deliberately never reads
	defer remote.Close()
	defer client.Close()
	client.dial = func(context.Context) (net.Conn, error) { return local, nil }
	started := time.Now()
	if err := client.Send([]byte("blocked")); err == nil {
		t.Fatal("write should time out")
	}
	if time.Since(started) > time.Second {
		t.Fatal("write exceeded bounded timeout")
	}
	client.gate <- struct{}{}
	started = time.Now()
	err = client.Send([]byte("busy"))
	<-client.gate
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("gate wait was unbounded: %v", err)
	}
}

func TestLoggingConfigurationValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"service with spaces", func(c *Config) { c.Service = "bad service" }},
		{"service too long", func(c *Config) { c.Service = strings.Repeat("x", 49) }},
		{"header injection", func(c *Config) { c.Instance = "host\nspoof" }},
		{"duplicate outputs", func(c *Config) { c.Outputs = []string{"stdout", "stdout"} }},
		{"unknown output", func(c *Config) { c.Outputs = []string{"file"} }},
		{"missing address", func(c *Config) { c.Syslog.Address = "" }},
		{"missing host", func(c *Config) { c.Syslog.Address = ":5140" }},
		{"invalid port", func(c *Config) { c.Syslog.Address = "localhost:0" }},
		{"unsupported transport", func(c *Config) { c.Syslog.Network = "udp" }},
		{"invalid facility", func(c *Config) { c.Syslog.Facility = "unknown" }},
		{"unbounded timeout", func(c *Config) { c.Syslog.Timeout = 0 }},
		{"TLS settings on plaintext", func(c *Config) { c.Syslog.TLSCAFile = "ca.pem" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Outputs = []string{"stdout", "syslog"}
			test.change(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("invalid logging configuration accepted")
			}
		})
	}
}

func TestEnvironmentSelectsApplicationAndOutputs(t *testing.T) {
	for _, key := range []string{"LOG_LEVEL", "LOG_SERVICE", "LOG_ENV", "LOG_INSTANCE", "LOG_OUTPUTS", "LOG_SYSLOG_ADDRESS", "LOG_SYSLOG_NETWORK", "LOG_SYSLOG_FACILITY", "LOG_SYSLOG_TIMEOUT", "LOG_SYSLOG_TLS_CA_FILE", "LOG_SYSLOG_TLS_SERVER_NAME"} {
		t.Setenv(key, "")
	}
	t.Setenv("LOG_SERVICE", "billing-api")
	t.Setenv("LOG_OUTPUTS", "stdout, syslog")
	t.Setenv("LOG_LEVEL", "warn")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Service != "billing-api" || len(cfg.Outputs) != 2 || cfg.Level != slog.LevelWarn {
		t.Fatalf("environment configuration ignored: %+v", cfg)
	}
	t.Setenv("LOG_SYSLOG_TIMEOUT", "forever")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("accepted malformed timeout")
	}
}
