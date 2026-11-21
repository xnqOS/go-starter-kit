package sendgrid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
	"github.com/xnqOS/go-starter-kit/kit/email/emailtest"
)

func TestContract(t *testing.T) {
	emailtest.Run(t, emailtest.Harness{
		APIKey: "SG.test",
		Path:   "/v3/mail/send",
		New: func(t *testing.T, baseURL string) email.Provider {
			p, err := New(Config{APIKey: "SG.test"})
			if err != nil {
				t.Fatal(err)
			}
			return p.WithBaseURL(baseURL)
		},
		Decode: func(t *testing.T, r *http.Request, body []byte) emailtest.Received {
			var req request
			if err := json.Unmarshal(body, &req); err != nil || len(req.Personalizations) != 1 || len(req.Personalizations[0].To) != 1 {
				t.Errorf("bad SendGrid request: %v", err)
				return emailtest.Received{}
			}
			to := req.Personalizations[0].To[0]
			out := emailtest.Received{From: req.From.Email, FromName: req.From.Name, To: to.Email, ToName: to.Name, Subject: req.Subject,
				Headers: req.Headers, Tags: req.CustomArgs, Authorization: r.Header.Get("Authorization"), IdempotencyKey: r.Header.Get("Idempotency-Key")}
			if req.ReplyTo != nil {
				out.ReplyTo, out.ReplyToName = req.ReplyTo.Email, req.ReplyTo.Name
			}
			for i, c := range req.Content {
				switch {
				case c.Type == "text/plain" && i == 0:
					out.Text = c.Value
				case c.Type == "text/html":
					out.HTML = c.Value
				default:
					t.Errorf("unexpected content %d: %s", i, c.Type)
				}
			}
			return out
		},
		Accept: func(w http.ResponseWriter, id string) {
			w.Header().Set("X-Message-Id", id)
			w.WriteHeader(http.StatusAccepted)
		},
		Reject: func(w http.ResponseWriter, status int) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":[{"message":"bad","field":"from.email"}]}`))
		},
	})
}

func keyPair(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, base64.StdEncoding.EncodeToString(der)
}

func signed(t *testing.T, key *ecdsa.PrivateKey, body string, at time.Time) *http.Request {
	t.Helper()
	stamp := strconv.FormatInt(at.Unix(), 10)
	hash := sha256.Sum256([]byte(stamp + body))
	sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set(signatureHeader, base64.StdEncoding.EncodeToString(sig))
	r.Header.Set(timestampHeader, stamp)
	return r
}

const events = `[
 {"email":"a@example.com","timestamp":1795000000,"event":"bounce","type":"bounce","sg_event_id":"e1","sg_message_id":"14c5d75ce93.dfd.64b469.filter0001.16648.5515E0B88.0","outbox_id":"o1","tenant_id":"t1","kind":"customer_password_reset"},
 {"email":"b@example.com","timestamp":1795000000,"event":"bounce","type":"blocked","sg_event_id":"e2","sg_message_id":"m2.filter"},
 {"email":"c@example.com","timestamp":1795000000,"event":"spamreport","sg_event_id":"e3","sg_message_id":"m3.filter"},
 {"email":"d@example.com","timestamp":1795000000,"event":"group_unsubscribe","sg_event_id":"e4","sg_message_id":"m4.filter"},
 {"email":"e@example.com","timestamp":1795000000,"event":"delivered","sg_event_id":"e5","sg_message_id":"m5.filter"},
 {"email":"f@example.com","timestamp":1795000000,"event":"processed","sg_event_id":"e6","sg_message_id":"m6.filter"}
]`

func TestWebhookVerifiesAndConvertsEvents(t *testing.T) {
	key, public := keyPair(t)
	now := time.Unix(1795000100, 0)
	p, err := New(Config{WebhookPublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	got, err := p.Parse(signed(t, key, events, now), []byte(events))
	if err != nil || len(got) != 5 {
		t.Fatalf("events %+v, err %v", got, err)
	}
	if e := got[0]; e.ID != "e1" || e.Type != email.EventBounced || !e.HardBounce || e.ProviderMessageID != "14c5d75ce93" || e.Recipient != "a@example.com" ||
		e.Tags["outbox_id"] != "o1" || e.Tags["tenant_id"] != "" || e.Tags["kind"] != "customer_password_reset" || !e.OccurredAt.Equal(time.Unix(1795000000, 0)) {
		t.Fatalf("hard bounce: %+v", e)
	}
	if got[1].HardBounce || got[2].Type != email.EventComplained || got[3].Type != email.EventUnsubscribed || got[4].Type != email.EventDelivered {
		t.Fatalf("event mapping: %+v", got)
	}
}

func TestWebhookRejectsForgedStaleAndUnconfigured(t *testing.T) {
	key, public := keyPair(t)
	other, _ := keyPair(t)
	now := time.Unix(1795000100, 0)
	p, _ := New(Config{WebhookPublicKey: public})
	p.now = func() time.Time { return now }
	for name, r := range map[string]*http.Request{
		"other key": signed(t, other, events, now),
		"stale":     signed(t, key, events, now.Add(-10*time.Minute)),
		"unsigned":  httptest.NewRequest(http.MethodPost, "/", strings.NewReader(events)),
	} {
		if _, err := p.Parse(r, []byte(events)); !errors.Is(err, email.ErrBadSignature) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := p.Parse(signed(t, key, events, now), []byte(events+" ")); !errors.Is(err, email.ErrBadSignature) {
		t.Fatal("modified body accepted")
	}
	unconfigured, _ := New(Config{})
	unconfigured.now = p.now
	if _, err := unconfigured.Parse(signed(t, key, events, now), []byte(events)); !errors.Is(err, email.ErrBadSignature) {
		t.Fatal("webhook accepted without a public key")
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("junk")), base64.StdEncoding.EncodeToString(rsaDER)} {
		if _, err := New(Config{WebhookPublicKey: bad}); err == nil {
			t.Fatalf("bad public key accepted: %q", bad)
		}
	}
}

func TestReservedCustomArgsAreNotSent(t *testing.T) {
	var got request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	p, _ := New(Config{APIKey: "k"})
	p.WithBaseURL(server.URL)
	m := email.Outbound{Subject: "s", Tags: map[string]string{"event": "x", "kind": "k"}}
	m.From.Address, m.To.Address = "a@example.com", "b@example.com"
	if _, err := p.Send(t.Context(), m); err != nil || got.CustomArgs["event"] != "" || got.CustomArgs["kind"] != "k" {
		t.Fatalf("custom args %v, err %v", got.CustomArgs, err)
	}
}
