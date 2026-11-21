// Package resend sends mail through the Resend HTTP API and parses its
// Svix-signed webhooks.
package resend

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

const defaultBaseURL = "https://api.resend.com"

type Config struct {
	APIKey string `json:"-"`
	// WebhookSecret is the signing secret shown by Resend ("whsec_…").
	WebhookSecret string `json:"-"`
}

type Provider struct {
	config  Config
	baseURL string
	client  *http.Client
	now     func() time.Time
}

func New(c Config) *Provider {
	return &Provider{config: c, baseURL: defaultBaseURL, client: &http.Client{}, now: time.Now}
}

// WithBaseURL points the adapter at a fake server. Tests only.
func (p *Provider) WithBaseURL(url string) *Provider {
	p.baseURL = strings.TrimRight(url, "/")
	return p
}

func (*Provider) Name() string { return "resend" }

// IdempotencyWindow: Resend drops a repeat of a request with the same
// Idempotency-Key for 24 hours.
// https://resend.com/docs/dashboard/emails/idempotency-keys
func (*Provider) IdempotencyWindow() time.Duration { return 24 * time.Hour }

type tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type request struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	Text    string            `json:"text"`
	HTML    string            `json:"html,omitempty"`
	ReplyTo []string          `json:"reply_to,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tags    []tag             `json:"tags,omitempty"`
}

// Resend only accepts tag names and values made of these characters.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func (p *Provider) Send(ctx context.Context, m email.Outbound) (email.Result, error) {
	if m.From.Address == "" || m.To.Address == "" || m.ValidateHeaders() != nil {
		return email.Result{}, email.Failed(email.Permanent, "invalid_headers", errors.New("invalid email headers"))
	}
	body := request{From: format(m.From), To: []string{format(m.To)}, Subject: m.Subject, Text: m.Text, HTML: m.HTML, Headers: m.Headers}
	if m.ReplyTo != nil {
		body.ReplyTo = []string{format(*m.ReplyTo)}
	}
	for name, value := range m.Tags {
		// Tags only help match webhooks; drop the ones Resend would reject
		// rather than failing the email.
		if tagPattern.MatchString(name) && tagPattern.MatchString(value) {
			body.Tags = append(body.Tags, tag{name, value})
		}
	}
	// Map order is random. Resend rejects a retry whose body differs from the
	// first request with the same Idempotency-Key, so the order must be fixed.
	sort.Slice(body.Tags, func(i, j int) bool { return body.Tags[i].Name < body.Tags[j].Name })
	header := http.Header{"Authorization": {"Bearer " + p.config.APIKey}}
	if m.IdempotencyKey != "" {
		if len(m.IdempotencyKey) > 256 {
			return email.Result{}, email.Failed(email.Permanent, "idempotency_key", errors.New("idempotency key longer than 256 characters"))
		}
		header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	resp, err := email.PostJSON(ctx, p.client, p.baseURL+"/emails", header, body)
	if err != nil {
		return email.Result{}, err
	}
	if resp.Status/100 == 2 {
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(resp.Body, &out)
		return email.Result{ProviderMessageID: out.ID}, nil
	}
	var failure struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(resp.Body, &failure)
	class := email.ClassifyStatus(resp.Status)
	if resp.Status == http.StatusConflict && failure.Name == "concurrent_idempotent_requests" {
		// Another request with the same key is still running; retry later.
		class = email.Retryable
	}
	return email.Result{}, email.StatusError(class, resp.Status, failure.Name)
}

// format writes "Name <a@b>" (RFC 5322-quoted) or a bare address.
func format(a mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return a.String()
}

// Parse verifies the Svix signature and converts Resend events.
func (p *Provider) Parse(r *http.Request, body []byte) ([]email.Event, error) {
	if err := p.verify(r.Header, body); err != nil {
		return nil, err
	}
	var payload struct {
		Type      string `json:"type"`
		CreatedAt string `json:"created_at"`
		Data      struct {
			EmailID   string          `json:"email_id"`
			To        []string        `json:"to"`
			CreatedAt string          `json:"created_at"`
			Tags      json.RawMessage `json:"tags"`
			Bounce    *struct {
				Type string `json:"type"`
			} `json:"bounce"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	event := email.Event{ID: r.Header.Get("svix-id"), ProviderMessageID: payload.Data.EmailID, Tags: parseTags(payload.Data.Tags)}
	switch payload.Type {
	case "email.delivered":
		event.Type = email.EventDelivered
	case "email.bounced":
		event.Type = email.EventBounced
		// Resend reports bounce.type Permanent, Transient or Undetermined.
		event.HardBounce = payload.Data.Bounce == nil || payload.Data.Bounce.Type == "Permanent"
	case "email.complained":
		event.Type = email.EventComplained
	case "email.opened":
		event.Type = email.EventOpened
	case "email.suppressed", "email.failed":
		event.Type = email.EventDropped
	default:
		return nil, nil
	}
	if len(payload.Data.To) > 0 {
		event.Recipient = payload.Data.To[0]
	}
	event.OccurredAt, _ = time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if event.OccurredAt.IsZero() {
		event.OccurredAt = p.now().UTC()
	}
	return []email.Event{event}, nil
}

// Tags arrive as {"name": "value"}; accept the [{name, value}] form we send too.
func parseTags(raw json.RawMessage) map[string]string {
	tags := map[string]string{}
	if len(raw) == 0 {
		return tags
	}
	if json.Unmarshal(raw, &tags) == nil {
		return tags
	}
	var list []tag
	if json.Unmarshal(raw, &list) == nil {
		for _, t := range list {
			tags[t.Name] = t.Value
		}
	}
	return tags
}

// verify implements Svix webhook signing: HMAC-SHA256 over
// "<svix-id>.<svix-timestamp>.<body>" with the base64 secret after "whsec_".
func (p *Provider) verify(header http.Header, body []byte) error {
	id, stamp, signatures := header.Get("svix-id"), header.Get("svix-timestamp"), header.Get("svix-signature")
	if p.config.WebhookSecret == "" || id == "" || stamp == "" || signatures == "" {
		return email.ErrBadSignature
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || !email.FreshTimestamp(time.Unix(seconds, 0), p.now()) {
		return email.ErrBadSignature
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(p.config.WebhookSecret, "whsec_"))
	if err != nil {
		return email.ErrBadSignature
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + stamp + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	// The header may carry several space-separated "v1,<base64>" signatures
	// while a secret is being rotated.
	for _, candidate := range strings.Fields(signatures) {
		version, sig, ok := strings.Cut(candidate, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return email.ErrBadSignature
}

// ValidWebhookSecret reports whether secret looks like a Svix secret.
func ValidWebhookSecret(secret string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	return err == nil && len(key) >= 16
}
