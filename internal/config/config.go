package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/xnqOS/go-starter-kit/kit/logging"
)

type Config struct {
	HTTPAddr    string
	Storage     string
	DatabaseURL string
	Log         logging.Config
	// TrustedProxies are the reverse proxies (IPs or CIDRs) whose
	// X-Forwarded-For is believed for the client IP. Empty trusts none.
	TrustedProxies []string
}

func Load() (Config, error) {
	logConfig, err := logging.LoadConfig()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		HTTPAddr:    valueOrDefault("HTTP_ADDR", ":8080"),
		Storage:     valueOrDefault("STORAGE", "sqlite"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Log:         logConfig,
	}
	if raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXY_CIDRS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if proxy := strings.TrimSpace(part); proxy != "" {
				cfg.TrustedProxies = append(cfg.TrustedProxies, proxy)
			}
		}
	}
	if cfg.Storage == "sqlite" && cfg.DatabaseURL == "" {
		cfg.DatabaseURL = "./data/app.db"
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Development says this is a developer's machine (LOG_ENV development or local).
func (c Config) Development() bool { return c.Log.Env == "development" || c.Log.Env == "local" }

func (c Config) Validate() error {
	for _, proxy := range c.TrustedProxies {
		if _, err := netip.ParseAddr(proxy); err != nil {
			if _, err := netip.ParsePrefix(proxy); err != nil {
				return fmt.Errorf("TRUSTED_PROXY_CIDRS must contain IP addresses or CIDRs")
			}
		}
	}
	if c.HTTPAddr == "" {
		return fmt.Errorf("HTTP_ADDR is required")
	}
	switch c.Storage {
	case "memory":
	case "sqlite", "postgres":
		if c.DatabaseURL == "" {
			return fmt.Errorf("DATABASE_URL is required when STORAGE=%s", c.Storage)
		}
	default:
		return fmt.Errorf("STORAGE must be sqlite, postgres or memory")
	}
	return nil
}

func valueOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
