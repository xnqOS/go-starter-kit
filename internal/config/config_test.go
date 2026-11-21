package config

import "testing"

func TestDefaultSQLiteAndStorageSelection(t *testing.T) {
	for _, key := range []string{"STORAGE", "DATABASE_URL", "HTTP_ADDR", "TRUSTED_PROXY_CIDRS", "LOG_OUTPUTS", "LOG_LEVEL", "LOG_SERVICE", "LOG_ENV"} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage != "sqlite" || cfg.DatabaseURL != "./data/app.db" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	t.Setenv("STORAGE", "postgres")
	if _, err := Load(); err == nil {
		t.Fatal("PostgreSQL accepted a missing connection URL")
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	cfg, err = Load()
	if err != nil || cfg.Storage != "postgres" {
		t.Fatalf("PostgreSQL configuration: %#v %v", cfg, err)
	}
	t.Setenv("STORAGE", "memory")
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedProxyConfiguration(t *testing.T) {
	t.Setenv("STORAGE", "memory")
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	cfg, err := Load()
	if err != nil || len(cfg.TrustedProxies) != 0 {
		t.Fatal("default must ignore forwarded headers")
	}
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 127.0.0.1, ::1/128")
	cfg, err = Load()
	if err != nil || len(cfg.TrustedProxies) != 3 {
		t.Fatalf("proxy parsing: %#v %v", cfg.TrustedProxies, err)
	}
	for _, bad := range []string{"proxy.example.com", "10.0.0.0/99", ""} {
		if bad == "" {
			continue
		}
		t.Setenv("TRUSTED_PROXY_CIDRS", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("invalid trusted proxy accepted: %q", bad)
		}
	}
	for _, raw := range []string{"10.0.0.0/8,", ",,10.0.0.0/8,, ", " , "} {
		t.Setenv("TRUSTED_PROXY_CIDRS", raw)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		expected := 1
		if raw == " , " {
			expected = 0
		}
		if len(cfg.TrustedProxies) != expected {
			t.Fatalf("unexpected proxies for %q: %v", raw, cfg.TrustedProxies)
		}
	}
}
