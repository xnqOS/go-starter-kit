package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
	"github.com/xnqOS/go-starter-kit/kit/logging"
)

// DecodeJSON enforces one JSON object and a 1 MiB request body limit.
func DecodeJSON(c *gin.Context, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		Reject(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return rejectJSON(c, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return rejectJSON(c, err)
	}
	return true
}

func rejectJSON(c *gin.Context, err error) bool {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		Reject(c, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 1 MiB")
	} else {
		Reject(c, http.StatusBadRequest, "invalid_json", "body must contain one JSON object with known fields")
	}
	return false
}

type errorKind struct {
	kind          error
	status        int
	code, message string
}

// errorKinds is checked in order; the first kind found in the chain decides
// the status, code and default message.
var errorKinds = []errorKind{
	{context.Canceled, 499, "request_canceled", "request canceled"},
	{context.DeadlineExceeded, http.StatusServiceUnavailable, "unavailable", "request deadline exceeded"},
	{apperror.ErrNotFound, http.StatusNotFound, "not_found", "resource not found"},
	{apperror.ErrInvalid, http.StatusBadRequest, "invalid_input", "invalid input"},
	{apperror.ErrUnauthorized, http.StatusUnauthorized, "unauthorized", "authentication required"},
	{apperror.ErrForbidden, http.StatusForbidden, "forbidden", "access denied"},
	{apperror.ErrConflict, http.StatusConflict, "conflict", "resource conflict"},
	{apperror.ErrRateLimited, http.StatusTooManyRequests, "rate_limited", "too many requests"},
	{apperror.ErrUnavailable, http.StatusServiceUnavailable, "unavailable", "service unavailable"},
}

// Fail maps err to an HTTP error response.
//
// Only an *apperror.Error can put its own code and message in the response.
// Plain wrapped errors such as fmt.Errorf("%w: email is required", apperror.ErrInvalid)
// get the kind's default message, so internal details never leak. To show a
// detail to clients, return apperror.Invalid("email is required") or apperror.New.
func Fail(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "internal_error", "an internal error occurred"
	var matched error
	for _, k := range errorKinds {
		if errors.Is(err, k.kind) {
			matched, status, code, message = k.kind, k.status, k.code, k.message
			break
		}
	}
	if matched == context.DeadlineExceeded || matched == apperror.ErrUnavailable {
		c.Header("Retry-After", "1")
	}
	if matched != nil {
		if public := publicError(err, matched); public != nil {
			if public.Code != "" {
				code = public.Code
			}
			if public.Message != "" {
				message = public.Message
			}
		}
	}
	if status >= http.StatusInternalServerError && matched != context.DeadlineExceeded {
		logging.FromContext(c.Request.Context()).ErrorContext(c.Request.Context(), "request failed", "error", err)
	}
	Reject(c, status, code, message)
}

// publicError finds the first *apperror.Error in the chain whose kind is
// kind, so its code and message always agree with the response status.
func publicError(err, kind error) *apperror.Error {
	var found *apperror.Error
	walk(err, func(e error) bool {
		public, ok := e.(*apperror.Error)
		if !ok {
			if custom, ok := e.(interface{ As(any) bool }); ok {
				if !custom.As(&public) {
					public = nil
				}
			}
		}
		if public != nil && errors.Is(public.Kind(), kind) {
			found = public
			return true
		}
		return false
	})
	return found
}

// walk visits err and everything it wraps, depth first, until visit returns true.
func walk(err error, visit func(error) bool) bool {
	if err == nil {
		return false
	}
	if visit(err) {
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() error }:
		return walk(e.Unwrap(), visit)
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if walk(inner, visit) {
				return true
			}
		}
	}
	return false
}
