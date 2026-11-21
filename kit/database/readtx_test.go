package database

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestSQLiteReadTxSkipsWriteLockAndCleansUp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "readtx.db")
	reader, err := Open(ctx, "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(reader)
	if err := reader.Exec("CREATE TABLE items(id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := reader.Exec("INSERT INTO items VALUES ('committed')").Error; err != nil {
		t.Fatal(err)
	}

	writer, err := Open(ctx, "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(writer)
	tx := writer.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Exec("INSERT INTO items VALUES ('uncommitted')").Error; err != nil {
		t.Fatal(err)
	}

	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var first, second int64
	err = ReadTx(readCtx, reader, func(tx *gorm.DB) error {
		if err := tx.Table("items").Count(&first).Error; err != nil {
			return err
		}
		return tx.Table("items").Count(&second).Error
	})
	if err != nil || first != 1 || second != 1 {
		t.Fatalf("read while writer holds lock: %d %d %v", first, second, err)
	}

	// A failed read must roll back and leave the single pooled connection
	// usable for writes once the other writer is done.
	boom := errors.New("boom")
	if err := ReadTx(ctx, reader, func(*gorm.DB) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("expected fn error, got %v", err)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if err := reader.Exec("INSERT INTO items VALUES ('after')").Error; err != nil {
		t.Fatalf("connection left inside a transaction: %v", err)
	}
}

func TestSQLiteReadTxReusesOuterTransaction(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "outer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	if err := db.Exec("CREATE TABLE items(id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	// With a second connection free, the bug would return stale data instead
	// of hanging; this catches both failure modes.
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(2)
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("INSERT INTO items VALUES ('uncommitted')").Error; err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	var count int64
	if err := ReadTx(readCtx, tx, func(tx *gorm.DB) error { return tx.Table("items").Count(&count).Error }); err != nil || count != 1 {
		t.Fatalf("ReadTx inside a transaction: count=%d %v", count, err)
	}
	// The outer transaction must still be open and usable afterwards.
	if err := tx.Exec("INSERT INTO items VALUES ('second')").Error; err != nil {
		t.Fatal(err)
	}
}

func sqliteReadFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Close(db) })
	if err := db.Exec("CREATE TABLE items(id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func assertSQLiteReadCleanup(t *testing.T, db *gorm.DB) {
	t.Helper()
	var queryOnly int
	if err := db.Raw("PRAGMA query_only").Scan(&queryOnly).Error; err != nil || queryOnly != 0 {
		t.Fatalf("query_only was not restored: %d %v", queryOnly, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Exec("INSERT INTO items VALUES ('after')").Error
	}); err != nil {
		t.Fatalf("pooled connection is not writable or remains in a transaction: %v", err)
	}
	if err := ReadTx(context.Background(), db, func(*gorm.DB) error { return nil }); err != nil {
		t.Fatalf("subsequent ReadTx failed: %v", err)
	}
}

func TestSQLiteReadTxControlFailures(t *testing.T) {
	for _, test := range []struct {
		name, failBefore, failAfter string
		failRollback, discard       bool
		callback                    bool
	}{
		{name: "failed commit rolls back", failBefore: "COMMIT", callback: true},
		{name: "ambiguous begin rolls back", failAfter: "BEGIN DEFERRED"},
		{name: "failed begin keeps clean connection", failBefore: "BEGIN DEFERRED"},
		{name: "ambiguous successful commit keeps clean connection", failAfter: "COMMIT", callback: true},
		{name: "failed commit and rollback discard connection", failBefore: "COMMIT", failRollback: true, discard: true, callback: true},
		{name: "failed callback and rollback discard connection", failRollback: true, discard: true, callback: true},
		{name: "failed pragma restoration discards connection", failBefore: "PRAGMA query_only = OFF", discard: true, callback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := sqliteReadFixture(t)
			fault := errors.New("injected control failure")
			callbackErr := errors.New("callback failed")
			rollbacks := 0
			if err := db.Callback().Raw().Before("gorm:raw").Register("test:before_control", func(tx *gorm.DB) {
				statement := tx.Statement.SQL.String()
				if statement == "ROLLBACK" {
					rollbacks++
				}
				if statement == test.failBefore || (test.failRollback && statement == "ROLLBACK") {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Raw().After("gorm:raw").Register("test:after_control", func(tx *gorm.DB) {
				if tx.Statement.SQL.String() == test.failAfter && tx.Error == nil {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			called := false
			err := ReadTx(context.Background(), db, func(*gorm.DB) error {
				called = true
				if test.failBefore == "" && test.failAfter == "" {
					return callbackErr
				}
				return nil
			})
			if !errors.Is(err, fault) || called != test.callback {
				t.Fatalf("control failure: callback=%v err=%v", called, err)
			}
			if test.failBefore == "" && test.failAfter == "" && !errors.Is(err, callbackErr) {
				t.Fatalf("cleanup lost callback error: %v", err)
			}
			wantRollbacks := 1
			if test.failBefore == "PRAGMA query_only = OFF" {
				wantRollbacks = 0 // COMMIT succeeded.
			}
			if rollbacks != wantRollbacks {
				t.Fatalf("rollback calls: got %d want %d", rollbacks, wantRollbacks)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			wantConnections := 1
			if test.discard {
				wantConnections = 0
			}
			if got := pool.Stats().OpenConnections; got != wantConnections {
				t.Fatalf("pooled connections: got %d want %d", got, wantConnections)
			}
			if err := db.Callback().Raw().Remove("test:before_control"); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Raw().Remove("test:after_control"); err != nil {
				t.Fatal(err)
			}
			assertSQLiteReadCleanup(t, db)
		})
	}
}

func TestSQLiteReadTxFailedBeginPreservesInMemoryDatabase(t *testing.T) {
	db, err := Open(context.Background(), "sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Close(db) })
	if err := db.Exec("CREATE TABLE items(id TEXT PRIMARY KEY); INSERT INTO items VALUES ('kept')").Error; err != nil {
		t.Fatal(err)
	}
	boom := errors.New("BEGIN failed without starting a transaction")
	if err := db.Callback().Raw().Before("gorm:raw").Register("test:fail_begin", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == "BEGIN DEFERRED" {
			tx.AddError(boom)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReadTx(context.Background(), db, func(*gorm.DB) error {
		t.Error("callback ran after failed BEGIN")
		return nil
	}); err != boom {
		t.Fatalf("failed BEGIN gained a spurious cleanup error: %v", err)
	}
	if err := db.Callback().Raw().Remove("test:fail_begin"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.Raw("SELECT id FROM items").Scan(&id).Error; err != nil || id != "kept" {
		t.Fatalf("in-memory schema or data lost after failed BEGIN: id=%q err=%v", id, err)
	}
	assertSQLiteReadCleanup(t, db)
}

func TestSQLiteReadTxCancellationDuringBegin(t *testing.T) {
	db := sqliteReadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := db.Callback().Raw().Before("gorm:raw").Register("test:cancel_begin", func(tx *gorm.DB) {
		switch tx.Statement.SQL.String() {
		case "BEGIN DEFERRED":
			cancel()
			fallthrough
		case "ROLLBACK":
			if tx.Statement.Context.Err() != nil {
				t.Error("transaction control inherited the cancelled request context")
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := ReadTx(ctx, db, func(tx *gorm.DB) error {
		if tx.Statement.Context.Err() != context.Canceled {
			t.Error("callback lost request cancellation")
		}
		return tx.Exec("SELECT 1").Error
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected request cancellation: %v", err)
	}
	if err := db.Callback().Raw().Remove("test:cancel_begin"); err != nil {
		t.Fatal(err)
	}
	assertSQLiteReadCleanup(t, db)
}

func TestSQLiteReadTxPanicCleansUp(t *testing.T) {
	db := sqliteReadFixture(t)
	boom := errors.New("callback panic")
	func() {
		defer func() {
			if recovered := recover(); recovered != boom {
				t.Errorf("panic was not preserved: %v", recovered)
			}
		}()
		_ = ReadTx(context.Background(), db, func(*gorm.DB) error { panic(boom) })
	}()
	assertSQLiteReadCleanup(t, db)
}

func TestSQLiteReadTxPreservesQueryOnly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			db := sqliteReadFixture(t)
			if err := db.Exec("PRAGMA query_only = ON").Error; err != nil {
				t.Fatal(err)
			}
			boom := errors.New("callback failed")
			err := ReadTx(context.Background(), db, func(*gorm.DB) error {
				if fail {
					return boom
				}
				return nil
			})
			if (fail && !errors.Is(err, boom)) || (!fail && err != nil) {
				t.Fatal(err)
			}
			var queryOnly int
			if err := db.Raw("PRAGMA query_only").Scan(&queryOnly).Error; err != nil || queryOnly != 1 {
				t.Fatalf("existing query_only policy lost: %d %v", queryOnly, err)
			}
		})
	}
}
