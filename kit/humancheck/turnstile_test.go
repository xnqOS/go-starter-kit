package humancheck

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

func TestTurnstile(t *testing.T) {
	var body, got string
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm.Get("secret") + "|" + r.PostForm.Get("response") + "|" + r.PostForm.Get("remoteip")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	verifier := func(allowTest bool) *Turnstile {
		v := NewTurnstile("secret-1", "https://Shops.test/", allowTest)
		v.Endpoint = server.URL
		return v
	}
	check := func(v *Turnstile) error {
		return v.Verify(context.Background(), "token-1", "203.0.113.5", "signup")
	}
	ok := `{"success":true,"hostname":"shops.test","action":"signup"}`
	for name, c := range map[string]struct {
		body      string
		allowTest bool
		pass      bool
	}{
		"real proof":             {ok, false, true},
		"failed":                 {`{"success":false,"error-codes":["timeout-or-duplicate"]}`, false, false},
		"other site":             {`{"success":true,"hostname":"evil.test","action":"signup"}`, false, false},
		"other action":           {`{"success":true,"hostname":"shops.test","action":"login"}`, false, false},
		"no action":              {`{"success":true,"hostname":"shops.test"}`, false, false},
		"testing key in dev":     {`{"success":true,"hostname":"example.com","metadata":{"result_with_testing_key":true}}`, true, true},
		"testing key in prod":    {`{"success":true,"hostname":"example.com","metadata":{"result_with_testing_key":true}}`, false, false},
		"testing key, real host": {`{"success":true,"hostname":"shops.test","action":"signup","metadata":{"result_with_testing_key":true}}`, false, false},
	} {
		body = c.body
		err := check(verifier(c.allowTest))
		if (err == nil) != c.pass || (err != nil && !errors.Is(err, ErrRejected)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got != "secret-1|token-1|203.0.113.5" {
		t.Fatalf("sent %q", got)
	}
	// Cloudflare trouble is "unavailable", never "not human".
	body, status = ok, 503
	if err := check(verifier(false)); !errors.Is(err, apperror.ErrUnavailable) {
		t.Fatalf("5xx: %v", err)
	}
	body, status = "not json", 200
	if err := check(verifier(false)); !errors.Is(err, apperror.ErrUnavailable) {
		t.Fatalf("bad body: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifier(false).Verify(ctx, "token-1", "203.0.113.5", "signup"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	server.Close()
	if err := check(verifier(false)); !errors.Is(err, apperror.ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
	if err := verifier(false).Verify(context.Background(), "", "203.0.113.5", "signup"); !errors.Is(err, ErrRejected) {
		t.Fatal("empty token accepted")
	}
}

func TestTurnstileInternalErrorIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["internal-error"]}`))
	}))
	defer server.Close()
	v := NewTurnstile("secret-1", "https://shops.test", false)
	v.Endpoint = server.URL
	if err := v.Verify(context.Background(), "token-1", "203.0.113.5", "signup"); !errors.Is(err, apperror.ErrUnavailable) || errors.Is(err, ErrRejected) {
		t.Fatalf("internal-error: %v", err)
	}
}

// endingBody stands in for a response body whose read fails because the
// request context ended after the headers had arrived.
type endingBody struct {
	ctx context.Context
	end func()
}

func (b endingBody) Read([]byte) (int, error) { b.end(); <-b.ctx.Done(); return 0, b.ctx.Err() }
func (endingBody) Close() error               { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTurnstileKeepsContextErrorWhileReadingBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{{"cancel", context.Canceled}, {"deadline", context.DeadlineExceeded}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			end := cancel
			if tc.want == context.DeadlineExceeded {
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
				end = func() {} // the deadline passes on its own
			}
			defer cancel()
			v := NewTurnstile("secret-1", "https://shops.test", false)
			v.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: endingBody{r.Context(), end}, Request: r}, nil
			})}
			if err := v.Verify(ctx, "token-1", "203.0.113.5", "signup"); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
