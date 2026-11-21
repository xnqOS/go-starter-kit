// Package sendgrid sends mail through the SendGrid v3 Mail Send API and parses
// its Signed Event Webhook.
package sendgrid

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

const defaultBaseURL = "https://api.sendgrid.com"

type Config struct {
	APIKey string `json:"-"`
	// WebhookPublicKey is the base64 verification key shown when the Signed
	// Event Webhook is turned on.
	WebhookPublicKey string
	// WebhookTags lists the custom args (Outbound.Tags) copied back into
	// Event.Tags. SendGrid puts custom args at the top level of each event,
	// next to its own fields, so only these names are read. Nil means
	// DefaultWebhookTags.
	WebhookTags []string
}

// DefaultWebhookTags are the tags the outbox needs to match an event to its
// row ("outbox_id") plus the message kind for logs.
var DefaultWebhookTags = []string{"kind", "outbox_id"}

type Provider struct {
	config  Config
	key     *ecdsa.PublicKey
	baseURL string
	client  *http.Client
	now     func() time.Time
}

// New fails only when a webhook public key is set but cannot be parsed.
func New(c Config) (*Provider, error) {
	p := &Provider{config: c, baseURL: defaultBaseURL, client: &http.Client{}, now: time.Now}
	if c.WebhookPublicKey != "" {
		key, err := ParsePublicKey(c.WebhookPublicKey)
		if err != nil {
			return nil, err
		}
		p.key = key
	}
	return p, nil
}

// WithBaseURL points the adapter at a fake server. Tests only.
func (p *Provider) WithBaseURL(url string) *Provider {
	p.baseURL = strings.TrimRight(url, "/")
	return p
}

func (*Provider) Name() string { return "sendgrid" }

type address struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type content struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type personalization struct {
	To []address `json:"to"`
}

type request struct {
	Personalizations []personalization `json:"personalizations"`
	From             address           `json:"from"`
	ReplyTo          *address          `json:"reply_to,omitempty"`
	Subject          string            `json:"subject"`
	Content          []content         `json:"content"`
	Headers          map[string]string `json:"headers,omitempty"`
	// CustomArgs come back as top-level fields of every webhook event.
	CustomArgs map[string]string `json:"custom_args,omitempty"`
}

// SendGrid adds these to webhook events itself; custom args with these names
// would be overwritten, so they are not sent.
var reservedArgs = map[string]bool{"email": true, "event": true, "timestamp": true, "sg_event_id": true, "sg_message_id": true, "type": true, "reason": true, "status": true}

// Send does not use IdempotencyKey: SendGrid has no idempotency support, so
// duplicates after an Ambiguous failure are handled by the caller.
func (p *Provider) Send(ctx context.Context, m email.Outbound) (email.Result, error) {
	if m.From.Address == "" || m.To.Address == "" || m.ValidateHeaders() != nil {
		return email.Result{}, email.Failed(email.Permanent, "invalid_headers", errors.New("invalid email headers"))
	}
	body := request{
		Personalizations: []personalization{{To: []address{{m.To.Address, m.To.Name}}}},
		From:             address{m.From.Address, m.From.Name},
		Subject:          m.Subject,
		// text/plain must come before text/html.
		Content: []content{{"text/plain", m.Text}},
		Headers: m.Headers,
	}
	if m.HTML != "" {
		body.Content = append(body.Content, content{"text/html", m.HTML})
	}
	if m.ReplyTo != nil {
		body.ReplyTo = &address{m.ReplyTo.Address, m.ReplyTo.Name}
	}
	for name, value := range m.Tags {
		if !reservedArgs[name] {
			if body.CustomArgs == nil {
				body.CustomArgs = map[string]string{}
			}
			body.CustomArgs[name] = value
		}
	}
	resp, err := email.PostJSON(ctx, p.client, p.baseURL+"/v3/mail/send", http.Header{"Authorization": {"Bearer " + p.config.APIKey}}, body)
	if err != nil {
		return email.Result{}, err
	}
	if resp.Status/100 == 2 {
		return email.Result{ProviderMessageID: resp.Header.Get("X-Message-Id")}, nil
	}
	var failure struct {
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(resp.Body, &failure)
	detail := ""
	if len(failure.Errors) > 0 && failure.Errors[0].Field != "" {
		detail = "field " + failure.Errors[0].Field
	}
	return email.Result{}, email.StatusError(email.ClassifyStatus(resp.Status), resp.Status, detail)
}

// ParsePublicKey decodes the base64 DER (PKIX) ECDSA key SendGrid shows.
func ParsePublicKey(raw string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("SendGrid webhook public key must be base64")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("SendGrid webhook public key is not a valid public key")
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("SendGrid webhook public key must be an ECDSA key")
	}
	return key, nil
}

const (
	signatureHeader = "X-Twilio-Email-Event-Webhook-Signature"
	timestampHeader = "X-Twilio-Email-Event-Webhook-Timestamp"
)

// Parse verifies the ECDSA signature over timestamp+body and converts events.
func (p *Provider) Parse(r *http.Request, body []byte) ([]email.Event, error) {
	if err := p.verify(r.Header, body); err != nil {
		return nil, err
	}
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	events := make([]email.Event, 0, len(raw))
	for _, item := range raw {
		event := email.Event{
			ID:        stringField(item, "sg_event_id"),
			Recipient: stringField(item, "email"),
			Tags:      map[string]string{},
		}
		switch stringField(item, "event") {
		case "delivered":
			event.Type = email.EventDelivered
		case "bounce":
			event.Type = email.EventBounced
			// "blocked" is a temporary rejection; only "bounce" is hard.
			event.HardBounce = stringField(item, "type") != "blocked"
		case "spamreport", "spam report":
			event.Type = email.EventComplained
		case "unsubscribe", "group_unsubscribe":
			event.Type = email.EventUnsubscribed
		case "open":
			event.Type = email.EventOpened
		case "dropped":
			event.Type = email.EventDropped
		default:
			continue
		}
		// sg_message_id is "<X-Message-Id>.filter…"; keep the part we stored.
		event.ProviderMessageID, _, _ = strings.Cut(stringField(item, "sg_message_id"), ".")
		if seconds, ok := item["timestamp"].(float64); ok {
			event.OccurredAt = time.Unix(int64(seconds), 0).UTC()
		} else {
			event.OccurredAt = p.now().UTC()
		}
		tags := p.config.WebhookTags
		if tags == nil {
			tags = DefaultWebhookTags
		}
		for _, name := range tags {
			if value := stringField(item, name); value != "" {
				event.Tags[name] = value
			}
		}
		events = append(events, event)
	}
	return events, nil
}

func stringField(item map[string]any, name string) string {
	value, _ := item[name].(string)
	return value
}

func (p *Provider) verify(header http.Header, body []byte) error {
	signature, stamp := header.Get(signatureHeader), header.Get(timestampHeader)
	if p.key == nil || signature == "" || stamp == "" {
		return email.ErrBadSignature
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || !email.FreshTimestamp(time.Unix(seconds, 0), p.now()) {
		return email.ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return email.ErrBadSignature
	}
	hash := sha256.New()
	hash.Write([]byte(stamp))
	hash.Write(body)
	if !ecdsa.VerifyASN1(p.key, hash.Sum(nil), sig) {
		return email.ErrBadSignature
	}
	return nil
}
