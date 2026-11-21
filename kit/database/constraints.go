package database

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-sqlite3"
)

type ConstraintKind uint8

const (
	ConstraintNone ConstraintKind = iota
	ConstraintUnique
	ConstraintForeignKey
	ConstraintCheck
)

// Constraint classifies driver errors without interpreting domain rules or
// exposing SQL details. Callers choose the appropriate public error message.
func Constraint(err error) ConstraintKind {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return ConstraintUnique
		case "23503":
			return ConstraintForeignKey
		case "23514":
			return ConstraintCheck
		}
	}
	var sq sqlite3.Error
	if errors.As(err, &sq) {
		switch sq.ExtendedCode {
		case sqlite3.ErrConstraintUnique, sqlite3.ErrConstraintPrimaryKey:
			return ConstraintUnique
		case sqlite3.ErrConstraintForeignKey:
			return ConstraintForeignKey
		case sqlite3.ErrConstraintCheck:
			return ConstraintCheck
		}
	}
	return ConstraintNone
}
