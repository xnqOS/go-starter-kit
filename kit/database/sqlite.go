package database

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func sqliteDSN(path string) (string, error) {
	if strings.HasPrefix(path, "file:") || strings.Contains(path, "://") {
		return "", fmt.Errorf("SQLite DATABASE_URL must be a file path or :memory:, not a connection URL")
	}
	dsn := ":memory:"
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("invalid SQLite file path")
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0755); err != nil {
			return "", fmt.Errorf("cannot create SQLite parent directory: %w", err)
		}
		dsn = (&url.URL{Scheme: "file", Path: absolute}).String()
	}
	return dsn + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_txlock=immediate", nil
}
