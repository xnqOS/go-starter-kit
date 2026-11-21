package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// SendTimeout bounds one provider request, matching the SMTP adapter.
const SendTimeout = 10 * time.Second

// HTTPResponse is the part of a provider's reply adapters need.
type HTTPResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// PostJSON sends body as JSON and classifies transport failures:
// failures before the request was fully written are Retryable, failures
// after it was written are Ambiguous (the provider may have accepted it).
// HTTP status codes are left to the caller, because their meaning differs per
// provider; see ClassifyStatus for the common rules.
func PostJSON(ctx context.Context, client *http.Client, url string, header http.Header, body any) (HTTPResponse, error) {
	return DoJSON(ctx, client, http.MethodPost, url, header, body)
}

// DoJSON is PostJSON for any method; a nil body sends no payload.
func DoJSON(ctx context.Context, client *http.Client, method, url string, header http.Header, body any) (HTTPResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return HTTPResponse{}, Failed(Permanent, "encode", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				written.Store(true)
			}
		},
	})
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return HTTPResponse{}, Failed(Permanent, "request", err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return HTTPResponse{}, transportError(ctx, err, written.Load())
	}
	defer resp.Body.Close()
	// Provider replies are small; cap what we read so a broken proxy cannot
	// make us buffer an unbounded body.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		// The status line arrived, so the provider saw the request.
		return HTTPResponse{}, transportError(ctx, err, true)
	}
	return HTTPResponse{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

func transportError(ctx context.Context, err error, written bool) error {
	code := "network"
	if ctxErr := ctx.Err(); ctxErr != nil {
		code, err = contextCode(ctxErr), ctxErr
	}
	if written {
		return Failed(Ambiguous, code, err)
	}
	return Failed(Retryable, code, err)
}

// ClassifyStatus applies the rules shared by HTTP providers to a non-2xx
// status: 429 and 408 are Retryable, 5xx are Ambiguous (the provider may have
// accepted the message before failing), other 4xx are Permanent.
func ClassifyStatus(status int) Class {
	switch {
	case status == http.StatusTooManyRequests || status == http.StatusRequestTimeout:
		return Retryable
	case status >= 500:
		return Ambiguous
	default:
		return Permanent
	}
}

// StatusError builds the classified error for a non-2xx reply. detail is a
// provider error name (never the request), kept short for logs.
func StatusError(class Class, status int, detail string) error {
	code := fmt.Sprintf("http_%d", status)
	if detail == "" {
		return Failed(class, code, nil)
	}
	if len(detail) > 120 {
		detail = detail[:120]
	}
	return Failed(class, code, errors.New(detail))
}
