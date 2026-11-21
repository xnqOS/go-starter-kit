package database

import (
	"context"
	"strings"
	"testing"
)

func TestSQLiteInMemoryConnection(t *testing.T) {
	db, err := Open(context.Background(), "sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	if err := db.Exec("CREATE TABLE test_memory (id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO test_memory(id) VALUES (?)", "kept").Error; err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.Raw("SELECT id FROM test_memory").Scan(&id).Error; err != nil || id != "kept" {
		t.Fatalf("in-memory connection lost: %s %v", id, err)
	}
}

func TestConnectionErrorsDoNotExposeCredentials(t *testing.T) {
	_, err := Open(context.Background(), "postgres", "host=localhost port=invalid password=top-secret")
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("unsafe connection error: %v", err)
	}
}
