package database

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

// ErrSavepointRecovery prevents callers from treating a failed rollback or
// release as a recoverable business error and committing a partial operation.
var ErrSavepointRecovery = errors.New("savepoint recovery failed")

// WithSavepoint never starts or commits a root transaction. A failed callback
// is rolled back even when its caller chooses to continue the outer transaction.
//
// If fn panics, the savepoint is rolled back and the panic continues. Should
// that cleanup fail, the panic value becomes an error wrapping
// ErrSavepointRecovery and the original panic, so a caller that recovers can
// tell the outer transaction must not be committed.
func WithSavepoint(tx *gorm.DB, fn func(*gorm.DB) error) (err error) {
	if tx == nil || tx.Statement == nil {
		return apperror.Invalid("an active transaction with savepoint support is required")
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok || tx.DisableNestedTransaction {
		return apperror.Invalid("an active transaction with savepoint support is required")
	}
	name := "business_" + strings.ToLower(rand.Text()) // Generated SQL identifier, never caller input.
	// Run the statements directly: gorm's SQLite and PostgreSQL dialectors
	// discard errors from SavePoint and RollbackTo, which would hide a failed
	// rollback and let the caller commit a partial operation.
	if e := tx.Exec("SAVEPOINT " + name).Error; e != nil {
		return errors.Join(ErrSavepointRecovery, e)
	}
	end := tx.WithContext(context.WithoutCancel(tx.Statement.Context))
	panicked := true
	defer func() {
		failed := false
		if panicked || err != nil {
			if e := end.Exec("ROLLBACK TO SAVEPOINT " + name).Error; e != nil {
				err = errors.Join(err, ErrSavepointRecovery, e)
				failed = true
			}
		}
		if !failed {
			if e := end.Exec("RELEASE SAVEPOINT " + name).Error; e != nil {
				err = errors.Join(err, ErrSavepointRecovery, e)
				failed = true
			}
		}
		// While fn panics, err never reaches the caller. If cleanup failed,
		// replace the panic with an error carrying ErrSavepointRecovery so a
		// caller that recovers does not commit the partial operation. When
		// cleanup succeeded the original panic keeps propagating untouched.
		if panicked && failed {
			cause := recover()
			original := fmt.Errorf("savepoint callback panicked: %v", cause)
			if e, ok := cause.(error); ok {
				original = fmt.Errorf("savepoint callback panicked: %w", e)
			}
			panic(errors.Join(original, err))
		}
	}()
	err = fn(tx)
	panicked = false
	if err == nil {
		err = tx.Statement.Context.Err()
	}
	return err
}
