// Package apperror contains errors shared by application logic and adapters.
package apperror

import "errors"

var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid input")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrRateLimited  = errors.New("rate limited")
	ErrUnavailable  = errors.New("unavailable")
)

// Error carries an explicitly public application code and message. Kind remains
// independent of HTTP and supports errors.Is through wrapping.
type Error struct {
	kind    error
	Code    string
	Message string
}

func New(kind error, code, message string) *Error {
	return &Error{kind: kind, Code: code, Message: message}
}

// Invalid returns a validation error with a public message. The code is left
// empty so httpx.Fail uses its default code for ErrInvalid.
func Invalid(message string) *Error { return New(ErrInvalid, "", message) }

// Kind returns the sentinel this error wraps, such as ErrInvalid.
func (e *Error) Kind() error { return e.kind }

// Error includes the kind and code so logs keep the error category. Clients
// only ever see Code and Message, through httpx.Fail.
func (e *Error) Error() string {
	text := e.Message
	if e.Code != "" {
		text = e.Code + ": " + text
	}
	if e.kind != nil {
		text = e.kind.Error() + ": " + text
	}
	return text
}

func (e *Error) Unwrap() error { return e.kind }
