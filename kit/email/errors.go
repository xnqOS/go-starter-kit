package email

import (
	"context"
	"errors"
	"fmt"
)

// Class says what a failed send means for retrying. Each adapter maps its own
// errors to a Class; callers only look at the Class.
type Class int

const (
	// Retryable: the provider certainly did not accept the message
	// (connection refused, 429, timeout before the request was sent).
	Retryable Class = iota + 1
	// Ambiguous: the provider may have accepted the message (timeout while
	// reading the response, connection lost after the body was written, 5xx).
	Ambiguous
	// Permanent: retrying will not help (invalid address, unverified domain,
	// bad credentials, rejected payload).
	Permanent
)

func (c Class) String() string {
	switch c {
	case Retryable:
		return "retryable"
	case Ambiguous:
		return "ambiguous"
	case Permanent:
		return "permanent"
	}
	return "unknown"
}

// Error is a classified send failure. Code is a short, log-safe reason such
// as "http_429" or "smtp_550"; it never contains message content or secrets.
type Error struct {
	Class Class
	Code  string
	Err   error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("email provider: %s (%s)", e.Code, e.Class)
	}
	return fmt.Sprintf("email provider: %s (%s): %v", e.Code, e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Classify returns the Class of err. Unclassified errors are Ambiguous,
// except context errors raised before anything was sent, which adapters
// classify themselves.
func Classify(err error) Class {
	if err == nil {
		return 0
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Class
	}
	if errors.Is(err, ErrDisabled) {
		return Permanent
	}
	return Ambiguous
}

// Code returns the log-safe code of err, or "" when err is not classified.
func Code(err error) string {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Code
	}
	return ""
}

// Failed wraps err with a class and code.
func Failed(class Class, code string, err error) error {
	return &Error{Class: class, Code: code, Err: err}
}

// contextCode names a context error for logs.
func contextCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "canceled"
}
