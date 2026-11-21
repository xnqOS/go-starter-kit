package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

func TestWithSavepointRollsBackOnlyTheFailedPart(t *testing.T) {
	for name, db := range testDatabases(t) {
		t.Run(name, func(t *testing.T) {
			if err := db.Exec("CREATE TABLE items (id TEXT PRIMARY KEY)").Error; err != nil {
				t.Fatal(err)
			}
			business := errors.New("business rule failed")
			err := db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec("INSERT INTO items VALUES ('kept')").Error; err != nil {
					return err
				}
				// A failed step is undone; the caller continues the outer transaction.
				err := WithSavepoint(tx, func(tx *gorm.DB) error {
					if err := tx.Exec("INSERT INTO items VALUES ('undone')").Error; err != nil {
						return err
					}
					return business
				})
				if !errors.Is(err, business) || errors.Is(err, ErrSavepointRecovery) {
					t.Fatalf("savepoint error: %v", err)
				}
				// A database error inside the savepoint leaves the transaction usable,
				// which matters on PostgreSQL where the error would abort it.
				err = WithSavepoint(tx, func(tx *gorm.DB) error {
					return tx.Exec("INSERT INTO items VALUES ('kept')").Error
				})
				if Constraint(err) != ConstraintUnique {
					t.Fatalf("expected unique violation, got %v", err)
				}
				return WithSavepoint(tx, func(tx *gorm.DB) error {
					return tx.Exec("INSERT INTO items VALUES ('also_kept')").Error
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			if err := db.Raw("SELECT id FROM items ORDER BY id").Scan(&ids).Error; err != nil {
				t.Fatal(err)
			}
			if len(ids) != 2 || ids[0] != "also_kept" || ids[1] != "kept" {
				t.Fatalf("rows after savepoints: %v", ids)
			}
		})
	}
}

func TestWithSavepointRequiresTransactionAndHandlesPanicAndCancel(t *testing.T) {
	db := testDatabases(t)["sqlite"]
	if err := WithSavepoint(db, func(*gorm.DB) error { return nil }); !errors.Is(err, apperror.ErrInvalid) {
		t.Fatalf("root db accepted: %v", err)
	}
	if err := db.Exec("CREATE TABLE items (id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	_ = db.Transaction(func(tx *gorm.DB) error {
		func() {
			defer func() { _ = recover() }()
			_ = WithSavepoint(tx, func(tx *gorm.DB) error {
				_ = tx.Exec("INSERT INTO items VALUES ('panic')").Error
				panic("boom")
			})
		}()
		return nil
	})
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM items").Scan(&n).Error; err != nil || n != 0 {
		t.Fatalf("panic left rows: %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return WithSavepoint(tx, func(tx *gorm.DB) error {
			if err := tx.Exec("INSERT INTO items VALUES ('cancelled')").Error; err != nil {
				return err
			}
			cancel()
			return nil
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled savepoint returned %v", err)
	}
}

// GORM's SQLite and PostgreSQL drivers ignore errors from SavePoint and
// RollbackTo, so failures of the control statements are injected here.
func TestWithSavepointReportsControlStatementFailures(t *testing.T) {
	for _, test := range []struct {
		name, fail   string
		fnErr        bool
		wantCallback bool
	}{
		{name: "create failure skips the callback", fail: "SAVEPOINT "},
		{name: "rollback failure is not recoverable", fail: "ROLLBACK TO SAVEPOINT ", fnErr: true, wantCallback: true},
		{name: "release failure is not recoverable", fail: "RELEASE SAVEPOINT ", wantCallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testDatabases(t)["sqlite"]
			if err := db.Exec("CREATE TABLE items (id TEXT PRIMARY KEY)").Error; err != nil {
				t.Fatal(err)
			}
			fault := errors.New("injected control failure")
			if err := db.Callback().Raw().Before("gorm:raw").Register("test:fail_control", func(tx *gorm.DB) {
				if strings.HasPrefix(tx.Statement.SQL.String(), test.fail) {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			business := errors.New("business error")
			called := false
			var err error
			_ = db.Transaction(func(tx *gorm.DB) error {
				err = WithSavepoint(tx, func(tx *gorm.DB) error {
					called = true
					if e := tx.Exec("INSERT INTO items VALUES ('partial')").Error; e != nil {
						return e
					}
					if test.fnErr {
						return business
					}
					return nil
				})
				return errors.New("discard the outer transaction")
			})
			if called != test.wantCallback {
				t.Fatalf("callback called = %v, want %v", called, test.wantCallback)
			}
			if !errors.Is(err, ErrSavepointRecovery) || !errors.Is(err, fault) {
				t.Fatalf("control failure not reported as unrecoverable: %v", err)
			}
			if test.fnErr && !errors.Is(err, business) {
				t.Fatalf("business error lost: %v", err)
			}
		})
	}
}

// A panicking callback skips the named result, so a failed cleanup must be
// carried by the panic itself; otherwise a caller that recovers inside the
// outer transaction would commit the partial operation.
func TestWithSavepointPanicCarriesCleanupFailure(t *testing.T) {
	business := errors.New("business panic")
	for _, test := range []struct{ name, fail string }{
		{name: "rollback succeeds"},
		{name: "rollback fails", fail: "ROLLBACK TO SAVEPOINT "},
		{name: "release fails", fail: "RELEASE SAVEPOINT "},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testDatabases(t)["sqlite"]
			if err := db.Exec("CREATE TABLE items (id TEXT PRIMARY KEY)").Error; err != nil {
				t.Fatal(err)
			}
			fault := errors.New("injected control failure")
			if test.fail != "" {
				if err := db.Callback().Raw().Before("gorm:raw").Register("test:fail_control", func(tx *gorm.DB) {
					if strings.HasPrefix(tx.Statement.SQL.String(), test.fail) {
						tx.AddError(fault)
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			var recovered any
			_ = db.Transaction(func(tx *gorm.DB) error {
				func() {
					defer func() { recovered = recover() }()
					_ = WithSavepoint(tx, func(tx *gorm.DB) error {
						_ = tx.Exec("INSERT INTO items VALUES ('partial')").Error
						panic(business)
					})
				}()
				// A caller that recovers decides from the panic value whether
				// the transaction can still be committed.
				if err, ok := recovered.(error); ok && errors.Is(err, ErrSavepointRecovery) {
					return err
				}
				return nil
			})
			if test.fail == "" {
				if recovered != business {
					t.Fatalf("successful cleanup changed the panic value: %#v", recovered)
				}
			} else {
				err, ok := recovered.(error)
				if !ok || !errors.Is(err, ErrSavepointRecovery) || !errors.Is(err, fault) || !errors.Is(err, business) {
					t.Fatalf("panic value lost the cleanup failure: %#v", recovered)
				}
			}
			var n int64
			if err := db.Raw("SELECT COUNT(*) FROM items").Scan(&n).Error; err != nil || n != 0 {
				t.Fatalf("partial operation committed: rows=%d err=%v", n, err)
			}
		})
	}
}
