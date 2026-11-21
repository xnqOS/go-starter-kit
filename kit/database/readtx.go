package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Mark only connections with an owned read transaction. Keeping the marker on
// ConnPool also preserves it across GORM's fresh-query (NewDB) session clones.
type sqliteReadConn struct{ *sql.Conn }

// ReadTx runs fn in a read-only snapshot transaction (for example a page of
// rows and its total count), or reuses the caller's existing transaction.
//
// PostgreSQL uses a read-only REPEATABLE READ transaction.
//
// SQLite connections are opened with _txlock=immediate, so a normal
// transaction would run BEGIN IMMEDIATE and take the write lock: a read would
// then wait on, and fail with "database is locked" behind, any writer in
// another process. ReadTx instead pins one connection and runs BEGIN DEFERRED,
// which under WAL reads a snapshot without blocking or waiting on writers.
//
// If db is already a transaction (a repository built with NewSQLRepo(tx), as
// docs/database.md recommends), fn runs on it directly: it then sees the
// transaction's own uncommitted writes and inherits its isolation and write
// policy. Nested calls must use the callback's tx, not the root db, to avoid
// waiting for another connection on SQLite's single-connection pool.
func ReadTx(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	db = db.WithContext(ctx)
	if db.Error != nil {
		return db.Error
	}
	switch db.Statement.ConnPool.(type) {
	case gorm.TxCommitter, *sqliteReadConn:
		return fn(db)
	}
	switch db.Dialector.Name() {
	case "postgres":
		return db.Transaction(fn, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	case "sqlite":
		return db.Connection(func(conn *gorm.DB) (err error) {
			raw := conn.Statement.ConnPool.(*sql.Conn)
			// Control statements must complete even if the request is cancelled
			// just as BEGIN succeeds. Queries in fn still use the request context.
			end := conn.WithContext(context.WithoutCancel(ctx))
			var queryOnly int
			if err := end.Raw("PRAGMA query_only").Scan(&queryOnly).Error; err != nil {
				return err
			}
			needsRollback := false
			defer func() {
				var cleanupErr error
				if needsRollback {
					cleanupErr = end.Exec("ROLLBACK").Error
					if cleanupErr != nil {
						// BEGIN may have failed before starting a transaction, or
						// SQLite may already have ended it. Check the driver's
						// actual state instead of matching error message text.
						_ = raw.Raw(func(driverConn any) error {
							if state, ok := driverConn.(interface{ AutoCommit() bool }); ok && state.AutoCommit() {
								cleanupErr = nil
							}
							return nil
						})
					}
				}
				restore := "PRAGMA query_only = OFF"
				if queryOnly != 0 {
					restore = "PRAGMA query_only = ON"
				}
				cleanupErr = errors.Join(cleanupErr, end.Exec(restore).Error)
				if cleanupErr != nil {
					// Returning ErrBadConn from this callback alone does not evict
					// the connection. Raw explicitly tells database/sql to discard it.
					_ = raw.Raw(func(any) error { return driver.ErrBadConn })
					err = errors.Join(err, fmt.Errorf("clean up SQLite read transaction: %w", cleanupErr))
				}
			}()
			if err := end.Exec("PRAGMA query_only = ON").Error; err != nil {
				return err
			}
			// Register rollback before BEGIN, including an ambiguous failure
			// where SQLite started the transaction but the driver returned an error.
			needsRollback = true
			if err := end.Exec("BEGIN DEFERRED").Error; err != nil {
				return err
			}
			read := conn.WithContext(ctx)
			read.Statement.ConnPool = &sqliteReadConn{raw}
			if err := fn(read); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := end.Exec("COMMIT").Error; err != nil {
				return err
			}
			needsRollback = false
			return nil
		})
	default:
		return db.Transaction(fn)
	}
}
