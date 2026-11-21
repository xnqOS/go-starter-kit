// Package providertest is the contract every HTTP email provider adapter must
// pass. A new provider only needs a Harness describing its wire format.
package emailtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"sync"
	"testing"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

// Received is a request as the fake provider understood it.
type Received struct {
	From, FromName       string
	To, ToName           string
	ReplyTo, ReplyToName string
	Subject, Text, HTML  string
	Headers, Tags        map[string]string
	IdempotencyKey       string
	Authorization        string
}

// Harness describes one provider's wire format.
type Harness struct {
	// New builds the adapter against a fake server at baseURL, using APIKey.
	New    func(t *testing.T, baseURL string) email.Provider
	APIKey string
	// Path is where the adapter must POST.
	Path string
	// Decode parses a captured request body.
	Decode func(t *testing.T, r *http.Request, body []byte) Received
	// Accept writes a success reply that carries message ID id.
	Accept func(w http.ResponseWriter, id string)
	// Reject writes an error reply with the given status.
	Reject func(w http.ResponseWriter, status int)
	// Idempotent: the adapter forwards IdempotencyKey, so retries with the
	// same key are deduplicated by the provider.
	Idempotent bool
}

type fake struct {
	t        *testing.T
	h        Harness
	mu       sync.Mutex
	got      []Received
	keys     map[string]string // idempotency key -> message ID
	bodies   map[string]string // idempotency key -> first request body
	raw      []string          // every request body, in order
	status   int               // non-zero: reject with this status
	hang     bool              // read the request, then never answer
	requests int
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.requests++
	if r.Method != http.MethodPost || r.URL.Path != f.h.Path {
		f.mu.Unlock()
		f.t.Errorf("request %s %s, want POST %s", r.Method, r.URL.Path, f.h.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	received := f.h.Decode(f.t, r, body)
	f.raw = append(f.raw, string(body))
	status, hang := f.status, f.hang
	// Like real providers, reusing an idempotency key with a different body
	// is an error, not a deduplicated retry.
	if key := received.IdempotencyKey; f.h.Idempotent && key != "" && status == 0 {
		if first, seen := f.bodies[key]; seen && first != string(body) {
			status = http.StatusConflict
		} else if !seen {
			f.bodies[key] = string(body)
		}
	}
	id := ""
	if status == 0 {
		if f.h.Idempotent && received.IdempotencyKey != "" && f.keys[received.IdempotencyKey] != "" {
			id = f.keys[received.IdempotencyKey]
		} else {
			f.got = append(f.got, received)
			id = "msg-" + string(rune('a'+len(f.got)-1))
			if received.IdempotencyKey != "" {
				f.keys[received.IdempotencyKey] = id
			}
		}
	}
	f.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	if status != 0 {
		f.h.Reject(w, status)
		return
	}
	f.h.Accept(w, id)
}

func (f *fake) set(fn func(*fake)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

func (f *fake) snapshot() ([]Received, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Received(nil), f.got...), f.requests
}

func start(t *testing.T, h Harness) (*fake, email.Provider) {
	f := &fake{t: t, h: h, keys: map[string]string{}, bodies: map[string]string{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, h.New(t, server.URL)
}

func message() email.Outbound {
	return email.Outbound{
		From:    mail.Address{Name: "茶小铺", Address: "no-reply@mail.example.com"},
		ReplyTo: &mail.Address{Name: "Owner", Address: "owner@example.com"},
		To:      mail.Address{Address: "buyer@example.com"},
		Subject: "Verify your email 你好", Text: "Token: abc\n", HTML: "<p>Token: abc</p>",
		Headers:        map[string]string{"List-Unsubscribe": "<https://example.com/u>"},
		IdempotencyKey: "outbox-1",
		Tags:           map[string]string{"kind": "customer_email_verification", "outbox_id": "o1", "source": "s1"},
	}
}

func wantClass(t *testing.T, err error, want email.Class) {
	t.Helper()
	if got := email.Classify(err); err == nil || got != want {
		t.Fatalf("error %v classified %s, want %s", err, got, want)
	}
}

// Run checks h's adapter against the contract.
func Run(t *testing.T, h Harness) {
	t.Run("delivers every field", func(t *testing.T) {
		f, p := start(t, h)
		result, err := p.Send(context.Background(), message())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := f.snapshot()
		if len(got) != 1 || result.ProviderMessageID != "msg-a" {
			t.Fatalf("deliveries %d, message ID %q", len(got), result.ProviderMessageID)
		}
		r := got[0]
		if r.From != "no-reply@mail.example.com" || r.FromName != "茶小铺" || r.To != "buyer@example.com" || r.ReplyTo != "owner@example.com" || r.ReplyToName != "Owner" {
			t.Fatalf("addresses: %+v", r)
		}
		if r.Subject != "Verify your email 你好" || r.Text != "Token: abc\n" || r.HTML != "<p>Token: abc</p>" || r.Headers["List-Unsubscribe"] != "<https://example.com/u>" {
			t.Fatalf("content: %+v", r)
		}
		if r.Tags["kind"] != "customer_email_verification" || r.Tags["outbox_id"] != "o1" || r.Tags["source"] != "s1" {
			t.Fatalf("tags: %v", r.Tags)
		}
		if r.Authorization != "Bearer "+h.APIKey {
			t.Fatal("API key not sent as a bearer token")
		}
		if h.Idempotent != (r.IdempotencyKey == "outbox-1") {
			t.Fatalf("idempotency key %q (provider idempotent: %v)", r.IdempotencyKey, h.Idempotent)
		}
	})

	t.Run("retries send identical bodies", func(t *testing.T) {
		f, p := start(t, h)
		// Tags and headers are maps; serializing them in map order would
		// change the body between retries of the same message.
		m := message()
		for i := range 8 {
			m.Tags[fmt.Sprintf("tag_%d", i)] = fmt.Sprintf("value_%d", i)
			m.Headers[fmt.Sprintf("X-Test-%d", i)] = fmt.Sprintf("%d", i)
		}
		for range 20 {
			if _, err := p.Send(context.Background(), m); err != nil {
				t.Fatalf("retry with the same idempotency key failed: %v", err)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, body := range f.raw {
			if body != f.raw[0] {
				t.Fatalf("request %d body differs from the first:\n%s\n%s", i, f.raw[0], body)
			}
		}
	})

	t.Run("text-only message has no HTML", func(t *testing.T) {
		f, p := start(t, h)
		m := message()
		m.HTML, m.ReplyTo, m.Headers = "", nil, nil
		if _, err := p.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		got, _ := f.snapshot()
		if got[0].HTML != "" || got[0].ReplyTo != "" || got[0].Text != "Token: abc\n" {
			t.Fatalf("text-only message: %+v", got[0])
		}
	})

	t.Run("rejects header injection without a request", func(t *testing.T) {
		f, p := start(t, h)
		for _, mutate := range []func(*email.Outbound){
			func(m *email.Outbound) { m.To.Address = "buyer@example.com\r\nBcc: x@example.com" },
			func(m *email.Outbound) { m.Subject = "hi\nBcc: x@example.com" },
			func(m *email.Outbound) { m.Headers = map[string]string{"X-A": "1\r\nBcc: x@example.com"} },
			func(m *email.Outbound) { m.From = mail.Address{} },
		} {
			m := message()
			mutate(&m)
			_, err := p.Send(context.Background(), m)
			wantClass(t, err, email.Permanent)
		}
		if _, requests := f.snapshot(); requests != 0 {
			t.Fatal("unsafe message reached the provider")
		}
	})

	t.Run("classifies status codes", func(t *testing.T) {
		f, p := start(t, h)
		for status, want := range map[int]email.Class{
			400: email.Permanent, 401: email.Permanent, 403: email.Permanent, 422: email.Permanent,
			429: email.Retryable, 500: email.Ambiguous, 503: email.Ambiguous,
		} {
			f.set(func(f *fake) { f.status = status })
			_, err := p.Send(context.Background(), message())
			wantClass(t, err, want)
			if code := email.Code(err); code == "" {
				t.Fatalf("status %d has no log code", status)
			}
		}
	})

	t.Run("connection refused is retryable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		url := "http://" + listener.Addr().String()
		_ = listener.Close()
		_, err = h.New(t, url).Send(context.Background(), message())
		wantClass(t, err, email.Retryable)
	})

	t.Run("cancelled before sending is retryable", func(t *testing.T) {
		f, p := start(t, h)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.Send(ctx, message())
		wantClass(t, err, email.Retryable)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost the context error: %v", err)
		}
		if _, requests := f.snapshot(); requests != 0 {
			t.Fatal("cancelled message reached the provider")
		}
	})

	t.Run("timeout after the provider received it is ambiguous", func(t *testing.T) {
		f, p := start(t, h)
		f.set(func(f *fake) { f.hang = true })
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := p.Send(ctx, message())
		wantClass(t, err, email.Ambiguous)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("deadline not honored: %v after %s", err, time.Since(start))
		}
		// Retrying with the same key: an idempotent provider keeps one copy.
		f.set(func(f *fake) { f.hang = false })
		if _, err := p.Send(context.Background(), message()); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Send(context.Background(), message()); err != nil {
			t.Fatal(err)
		}
		got, _ := f.snapshot()
		want := 3 // the hung request was recorded too
		if h.Idempotent {
			want = 1
		}
		if len(got) != want {
			t.Fatalf("provider recorded %d deliveries, want %d", len(got), want)
		}
	})
}
