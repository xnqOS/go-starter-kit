package logging

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// This opt-in test checks concurrent instances against a real server, including
// RFC 5424 identity, framing, Unicode and structured JSON query results.
func TestVictoriaLogsIntegration(t *testing.T) {
	serverURL := os.Getenv("VICTORIALOGS_TEST_URL")
	address := os.Getenv("VICTORIALOGS_TEST_SYSLOG_ADDRESS")
	if serverURL == "" && address == "" {
		t.Skip("run make logs-test against a local VictoriaLogs server")
	}
	if serverURL == "" || address == "" {
		t.Fatal("set both VICTORIALOGS_TEST_URL and VICTORIALOGS_TEST_SYSLOG_ADDRESS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	token := rand.Text()
	const message = "多实例日志\nsecond line"
	services := []string{"smoke-a-" + token, "smoke-b-" + token}
	instances := []string{"worker-1", "worker-2"}
	expected := make(map[string]bool)
	var writers sync.WaitGroup
	failures := make(chan error, len(services)*len(instances))
	for _, service := range services {
		for _, instance := range instances {
			for sequence := range 3 {
				expected[fmt.Sprintf("%s/%s/%d", service, instance, sequence)] = true
			}
			writers.Go(func() {
				failures <- func() error {
					cfg := DefaultConfig()
					cfg.Outputs = []string{"syslog"}
					cfg.Service, cfg.Env, cfg.Instance = service, "integration", instance
					cfg.Syslog.Address = address
					cfg.Syslog.Timeout = 2 * time.Second
					if network := os.Getenv("VICTORIALOGS_TEST_SYSLOG_NETWORK"); network != "" {
						cfg.Syslog.Network = network
					}
					cfg.Syslog.TLSCAFile = os.Getenv("VICTORIALOGS_TEST_TLS_CA_FILE")
					cfg.Syslog.TLSServerName = os.Getenv("VICTORIALOGS_TEST_TLS_SERVER_NAME")
					logger, closeLogs, err := New(cfg)
					if err != nil {
						return err
					}
					for sequence := range 3 {
						requestID := fmt.Sprintf("%s/%s/%d", service, instance, sequence)
						record := slog.NewRecord(time.Now(), slog.LevelInfo, message, 0)
						record.AddAttrs(slog.String("smoke_token", token), slog.String("request_id", requestID))
						// slog methods discard errors; inspect the Handler result.
						if err := logger.Handler().Handle(ctx, record); err != nil {
							_ = closeLogs()
							return err
						}
					}
					return closeLogs()
				}()
			})
		}
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Timeout: 2 * time.Second}
	query := fmt.Sprintf(`_time:5m | unpack_json | filter smoke_token:=%q`, token)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/select/logsql/query", strings.NewReader(url.Values{"query": {query}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != 200 {
			t.Fatalf("query failed: %s", body)
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		seen := make(map[string]bool)
		for {
			var row struct {
				AppName   string `json:"app_name"`
				Hostname  string `json:"hostname"`
				Service   string `json:"service"`
				Instance  string `json:"instance"`
				Env       string `json:"env"`
				Message   string `json:"msg"`
				RequestID string `json:"request_id"`
			}
			if err := decoder.Decode(&row); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("invalid query JSON: %v", err)
			}
			if !expected[row.RequestID] || seen[row.RequestID] || row.Service != row.AppName || row.Instance != row.Hostname || row.Env != "integration" || row.Message != message || !strings.HasPrefix(row.RequestID, row.AppName+"/"+row.Hostname+"/") {
				t.Fatalf("lost, duplicated or mixed instance fields: %+v", row)
			}
			seen[row.RequestID] = true
		}
		if len(seen) == len(expected) {
			t.Logf("verified %d logs from %d services × %d concurrent instances", len(seen), len(services), len(instances))
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("server indexed %d/%d logs: %s", len(seen), len(expected), body)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
