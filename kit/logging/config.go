// Package logging connects the standard slog API to console and Syslog outputs.
package logging

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Level    slog.Level
	Service  string
	Env      string
	Instance string
	Outputs  []string
	Syslog   SyslogConfig
}

type SyslogConfig struct {
	Network       string
	Address       string
	Facility      string
	Timeout       time.Duration
	TLSCAFile     string
	TLSServerName string
}

func DefaultConfig() Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return Config{
		Level: slog.LevelInfo, Service: "starter-kit", Env: "development",
		Instance: hostname, Outputs: []string{"stdout"},
		Syslog: SyslogConfig{
			Network: "tcp", Address: "127.0.0.1:5140", Facility: "local0", Timeout: 500 * time.Millisecond,
		},
	}
}

func LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	if value := os.Getenv("LOG_LEVEL"); value != "" {
		if err := cfg.Level.UnmarshalText([]byte(value)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	cfg.Service = envOr("LOG_SERVICE", cfg.Service)
	cfg.Env = envOr("LOG_ENV", cfg.Env)
	cfg.Instance = envOr("LOG_INSTANCE", cfg.Instance)
	if value := os.Getenv("LOG_OUTPUTS"); value != "" {
		cfg.Outputs = strings.Split(value, ",")
		for i := range cfg.Outputs {
			cfg.Outputs[i] = strings.TrimSpace(cfg.Outputs[i])
		}
	}
	cfg.Syslog.Network = envOr("LOG_SYSLOG_NETWORK", cfg.Syslog.Network)
	cfg.Syslog.Address = envOr("LOG_SYSLOG_ADDRESS", cfg.Syslog.Address)
	cfg.Syslog.Facility = envOr("LOG_SYSLOG_FACILITY", cfg.Syslog.Facility)
	cfg.Syslog.TLSCAFile = os.Getenv("LOG_SYSLOG_TLS_CA_FILE")
	cfg.Syslog.TLSServerName = os.Getenv("LOG_SYSLOG_TLS_SERVER_NAME")
	if value := os.Getenv("LOG_SYSLOG_TIMEOUT"); value != "" {
		var err error
		cfg.Syslog.Timeout, err = time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("LOG_SYSLOG_TIMEOUT: %w", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if !headerValue(c.Service, 48) || c.Service == "-" {
		return fmt.Errorf("LOG_SERVICE must contain 1–48 printable ASCII characters without spaces and cannot be '-'")
	}
	if !headerValue(c.Instance, 255) || c.Instance == "-" {
		return fmt.Errorf("LOG_INSTANCE must contain 1–255 printable ASCII characters without spaces and cannot be '-'")
	}
	if len(c.Outputs) == 0 {
		return fmt.Errorf("LOG_OUTPUTS must contain stdout or syslog")
	}
	seen := map[string]bool{}
	for _, output := range c.Outputs {
		if (output != "stdout" && output != "syslog") || seen[output] {
			return fmt.Errorf("LOG_OUTPUTS must contain unique outputs chosen from stdout,syslog")
		}
		seen[output] = true
	}
	if !seen["syslog"] {
		return nil
	}
	if c.Syslog.Network != "tcp" && c.Syslog.Network != "tls" {
		return fmt.Errorf("LOG_SYSLOG_NETWORK must be tcp or tls")
	}
	host, port, err := net.SplitHostPort(c.Syslog.Address)
	if err != nil || host == "" || !headerValue(host, 255) {
		return fmt.Errorf("LOG_SYSLOG_ADDRESS must be a host:port address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("LOG_SYSLOG_ADDRESS must use a port between 1 and 65535")
	}
	if c.Syslog.Timeout <= 0 || c.Syslog.Timeout > 30*time.Second {
		return fmt.Errorf("LOG_SYSLOG_TIMEOUT must be greater than 0 and at most 30s")
	}
	if _, ok := facilities[c.Syslog.Facility]; !ok {
		return fmt.Errorf("LOG_SYSLOG_FACILITY must be user, daemon, or local0 through local7")
	}
	if c.Syslog.Network != "tls" && (c.Syslog.TLSCAFile != "" || c.Syslog.TLSServerName != "") {
		return fmt.Errorf("LOG_SYSLOG_TLS_* settings require LOG_SYSLOG_NETWORK=tls")
	}
	return nil
}

func headerValue(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for i := range len(value) {
		if value[i] < 33 || value[i] > 126 {
			return false
		}
	}
	return true
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
