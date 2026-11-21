package database

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestSQLitePathAndConnectionSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "test #1?.db")
	dsn, err := sqliteDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Path != path {
		t.Fatalf("file path changed: %s %v", dsn, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]string{"_busy_timeout": "5000", "_foreign_keys": "on", "_journal_mode": "WAL", "_txlock": "immediate"} {
		if parsed.Query().Get(key) != expected {
			t.Fatalf("missing connection option %s", key)
		}
	}
	for _, invalid := range []string{"postgres://user:secret@localhost/db", "file:test.db?mode=ro"} {
		if _, err := sqliteDSN(invalid); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	// Verify URI escaping against the real driver, including '#' and '?'.
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("driver opened the wrong file: %v", err)
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode: %s %v", journal, err)
	}
	if _, err := db.Exec("CREATE TABLE parents(id TEXT PRIMARY KEY); CREATE TABLE children(parent_id TEXT REFERENCES parents(id))"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO children(parent_id) VALUES ('missing')"); err == nil {
		t.Fatal("foreign key not enforced")
	}
}

func TestSQLiteImmediateTransactionSerializesWriters(t *testing.T) {
	dsn, err := sqliteDSN(filepath.Join(t.TempDir(), "locking.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	tx, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := second.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	if other, err := second.BeginTx(ctx, nil); err == nil {
		_ = other.Rollback()
		t.Fatal("second writer acquired a transaction while the first held the lock")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	other, err := second.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("writer could not proceed after unlock: %v", err)
	}
	if err := other.Rollback(); err != nil {
		t.Fatal(err)
	}
}
