package resend

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
	"github.com/xnqOS/go-starter-kit/kit/email/emailtest"
)

func address(t *testing.T, raw string) (string, string) {
	t.Helper()
	parsed, err := mail.ParseAddress(raw)
	if err != nil {
		t.Fatalf("Resend address %q: %v", raw, err)
	}
	return parsed.Address, parsed.Name
}

func TestContract(t *testing.T) {
	emailtest.Run(t, emailtest.Harness{
		APIKey: "re_test",
		Path:   "/emails",
		New: func(t *testing.T, baseURL string) email.Provider {
			return New(Config{APIKey: "re_test"}).WithBaseURL(baseURL)
		},
		Idempotent: true,
		Decode: func(t *testing.T, r *http.Request, body []byte) emailtest.Received {
			var req request
			if err := json.Unmarshal(body, &req); err != nil || len(req.To) != 1 {
				t.Errorf("bad Resend request: %v", err)
				return emailtest.Received{}
			}
			out := emailtest.Received{Subject: req.Subject, Text: req.Text, HTML: req.HTML, Headers: req.Headers, Tags: map[string]string{},
				IdempotencyKey: r.Header.Get("Idempotency-Key"), Authorization: r.Header.Get("Authorization")}
			out.From, out.FromName = address(t, req.From)
			out.To, out.ToName = address(t, req.To[0])
			if len(req.ReplyTo) == 1 {
				out.ReplyTo, out.ReplyToName = address(t, req.ReplyTo[0])
			}
			for _, tag := range req.Tags {
				out.Tags[tag.Name] = tag.Value
			}
			return out
		},
		Accept: func(w http.ResponseWriter, id string) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q}`, id)
		},
		Reject: func(w http.ResponseWriter, status int) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"statusCode":%d,"name":"validation_error","message":"nope"}`, status)
		},
	})
}

func TestConcurrentIdempotentRequestIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"name":"concurrent_idempotent_requests"}`)
	}))
	defer server.Close()
	p := New(Config{APIKey: "k"}).WithBaseURL(server.URL)
	_, err := p.Send(t.Context(), email.Outbound{From: mail.Address{Address: "a@example.com"}, To: mail.Address{Address: "b@example.com"}, Subject: "s", IdempotencyKey: "k1"})
	if email.Classify(err) != email.Retryable {
		t.Fatalf("concurrent idempotent request: %v", err)
	}
}

func TestInvalidTagsAreDropped(t *testing.T) {
	var got request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"id":"x"}`)
	}))
	defer server.Close()
	p := New(Config{APIKey: "k"}).WithBaseURL(server.URL)
	_, err := p.Send(t.Context(), email.Outbound{From: mail.Address{Address: "a@example.com"}, To: mail.Address{Address: "b@example.com"}, Subject: "s",
		Tags: map[string]string{"kind": "ok", "tenant_id": "has space", "x.y": "z"}})
	if err != nil || len(got.Tags) != 1 || got.Tags[0] != (tag{"kind", "ok"}) {
		t.Fatalf("tags %v, err %v", got.Tags, err)
	}
}

// secret is a test-only Svix-style webhook secret: "whsec_" plus a base64 key.
// It is assembled so secret scanners do not mistake it for a real one.
var secret = "whsec" + "_" + base64.StdEncoding.EncodeToString([]byte("kit-resend-webhook-test-key-0001"))

func signed(t *testing.T, body string, at time.Time, secretValue string) *http.Request {
	t.Helper()
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(secretValue, "whsec_"))
	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("msg_1." + stamp + "." + body))
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("svix-id", "msg_1")
	r.Header.Set("svix-timestamp", stamp)
	r.Header.Set("svix-signature", "v1,bm90LXRoaXM= v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return r
}

func TestWebhookVerifiesAndConvertsEvents(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	p := New(Config{WebhookSecret: secret})
	p.now = func() time.Time { return now }
	body := `{"type":"email.bounced","created_at":"2026-11-18T09:59:00.000Z","data":{"email_id":"em_1","to":["buyer@example.com"],"tags":{"kind":"customer_password_reset","tenant_id":"t1"},"bounce":{"type":"Permanent","subType":"General"}}}`
	events, err := p.Parse(signed(t, body, now, secret), []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("events %v, err %v", events, err)
	}
	e := events[0]
	if e.ID != "msg_1" || e.Type != email.EventBounced || !e.HardBounce || e.ProviderMessageID != "em_1" || e.Recipient != "buyer@example.com" || e.Tags["tenant_id"] != "t1" || !e.OccurredAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("event: %+v", e)
	}
	soft := strings.Replace(body, `"Permanent"`, `"Transient"`, 1)
	if events, _ := p.Parse(signed(t, soft, now, secret), []byte(soft)); len(events) != 1 || events[0].HardBounce {
		t.Fatal("transient bounce treated as hard")
	}
	for typ, want := range map[string]email.EventType{"email.delivered": email.EventDelivered, "email.complained": email.EventComplained, "email.opened": email.EventOpened} {
		other := strings.Replace(body, "email.bounced", typ, 1)
		if events, err := p.Parse(signed(t, other, now, secret), []byte(other)); err != nil || len(events) != 1 || events[0].Type != want {
			t.Fatalf("%s: %v %v", typ, events, err)
		}
	}
	ignored := strings.Replace(body, "email.bounced", "email.clicked", 1)
	if events, err := p.Parse(signed(t, ignored, now, secret), []byte(ignored)); err != nil || len(events) != 0 {
		t.Fatal("clicks should be ignored")
	}
}

func TestWebhookRejectsForgedStaleAndUnconfigured(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	p := New(Config{WebhookSecret: secret})
	p.now = func() time.Time { return now }
	body := `{"type":"email.delivered","data":{"email_id":"em_1"}}`
	for name, r := range map[string]*http.Request{
		"other secret": signed(t, body, now, "whsec_"+base64.StdEncoding.EncodeToString([]byte("another-secret-value"))),
		"stale":        signed(t, body, now.Add(-10*time.Minute), secret),
		"future":       signed(t, body, now.Add(10*time.Minute), secret),
		"unsigned":     httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)),
	} {
		if _, err := p.Parse(r, []byte(body)); !errors.Is(err, email.ErrBadSignature) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := p.Parse(signed(t, body, now, secret), []byte(body+" ")); !errors.Is(err, email.ErrBadSignature) {
		t.Fatal("modified body accepted")
	}
	unconfigured := New(Config{})
	unconfigured.now = p.now
	if _, err := unconfigured.Parse(signed(t, body, now, secret), []byte(body)); !errors.Is(err, email.ErrBadSignature) {
		t.Fatal("webhook accepted without a configured secret")
	}
	if !ValidWebhookSecret(secret) || ValidWebhookSecret("whsec_short") || ValidWebhookSecret("not base64!") {
		t.Fatal("secret validation")
	}
}
