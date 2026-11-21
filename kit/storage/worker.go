package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultMaxSize is the largest object Worker stores or reads by default.
const DefaultMaxSize = 32 << 20

// WorkerOptions tune a Worker; zero values take the defaults.
type WorkerOptions struct {
	// Client defaults to an http.Client with a 30s timeout.
	Client *http.Client
	// MaxSize bounds Put and Open (default DefaultMaxSize). It must not
	// exceed what the Worker itself accepts.
	MaxSize int64
}

// Worker stores objects through a small HTTP service, typically a Cloudflare
// Worker in front of an R2 bucket. The protocol:
//
//	PUT    /objects/<key>  201 created, 409 if the key exists
//	GET    /objects/<key>  200 with the body, 404 if missing
//	DELETE /objects/<key>  204, also when already gone (404 is accepted too)
//
// Every request carries "Authorization: Bearer <secret>". The service only
// talks to the API; the API keeps checking permissions before every read.
type Worker struct {
	base    string
	secret  string
	client  *http.Client
	maxSize int64
}

// NewWorker takes the service's base URL (https, or http on localhost for
// development) and the shared secret (at least 32 characters).
func NewWorker(baseURL, secret string, opts WorkerOptions) (*Worker, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("storage worker URL must be an absolute URL without credentials, query or fragment")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, errors.New("storage worker URL must use https (http only for localhost)")
	}
	if len(secret) < 32 {
		return nil, errors.New("storage worker secret must be at least 32 characters")
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.MaxSize <= 0 {
		opts.MaxSize = DefaultMaxSize
	}
	return &Worker{base: strings.TrimRight(u.String(), "/"), secret: secret, client: opts.Client, maxSize: opts.MaxSize}, nil
}

func (s *Worker) do(ctx context.Context, method, key string, body []byte) (*http.Response, error) {
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+"/objects/"+key, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", ContentType(key))
	}
	return s.client.Do(req)
}

// workerError reads a short reason from a failed response.
func workerError(op string, resp *http.Response) error {
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("storage worker %s: HTTP %d: %s", op, resp.StatusCode, strings.TrimSpace(string(detail)))
}

func (s *Worker) Put(ctx context.Context, key string, data []byte) error {
	if len(data) == 0 || int64(len(data)) > s.maxSize {
		return fmt.Errorf("object of %d bytes cannot be stored", len(data))
	}
	resp, err := s.do(ctx, http.MethodPut, key, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrExists, workerError("put", resp))
	}
	return workerError("put", resp)
}

type readSeekNopCloser struct{ *bytes.Reader }

func (readSeekNopCloser) Close() error { return nil }

// Open buffers the object (at most MaxSize) so the result can seek, as
// http.ServeContent needs.
func (s *Worker) Open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, workerError("get", resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxSize {
		return nil, errors.New("storage worker returned an oversized object")
	}
	return readSeekNopCloser{bytes.NewReader(data)}, nil
}

func (s *Worker) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return workerError("delete", resp)
	}
	return nil
}
