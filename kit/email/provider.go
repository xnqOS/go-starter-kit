// Package email defines the boundary between an application's mail code and
// whichever company actually delivers the mail. Each provider has an adapter
// in a subpackage (smtp, resend, sendgrid); application code depends on the
// Provider interface, and the outbox subpackage queues and retries sends.
package email

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"strings"
	"time"
)

// Provider delivers one fully rendered message. Implementations must honor
// context cancellation and deadlines.
type Provider interface {
	Name() string
	Send(ctx context.Context, m Outbound) (Result, error)
}

// Outbound is a message ready to hand to a provider: addresses are parsed and
// the body is rendered. Adapters must not change content.
type Outbound struct {
	From    mail.Address
	ReplyTo *mail.Address // optional
	To      mail.Address
	Subject string
	Text    string
	HTML    string            // optional; sent as multipart/alternative with Text
	Headers map[string]string // optional extra headers, e.g. List-Unsubscribe
	// IdempotencyKey is passed to providers that support one.
	IdempotencyKey string
	// Tags are opaque labels (for example kind and outbox_id) for providers that
	// echo them back in webhooks. Adapters that cannot send them drop them.
	Tags map[string]string
}

// Result is what a provider tells us about an accepted message.
type Result struct {
	// ProviderMessageID is empty when the provider does not return one (SMTP).
	ProviderMessageID string
}

// ErrDisabled is returned by Disabled.
var ErrDisabled = errors.New("outbound email is disabled; configure a mail provider")

// CanDeliver reports whether p can actually deliver mail. Providers opt out by
// implementing CanDeliver() bool.
func CanDeliver(p Provider) bool {
	if capability, ok := p.(interface{ CanDeliver() bool }); ok {
		return capability.CanDeliver()
	}
	return p != nil
}

// IdempotencyWindow is how long p deduplicates sends that carry the same
// IdempotencyKey: a repeat that reaches the provider within this time of the
// first accepted request is dropped, so a retry after an ambiguous failure
// cannot deliver twice. Zero means p does not deduplicate. Providers opt in by
// implementing IdempotencyWindow() time.Duration; one that deduplicates
// forever returns a very large duration.
func IdempotencyWindow(p Provider) time.Duration {
	if capability, ok := p.(interface{ IdempotencyWindow() time.Duration }); ok {
		return max(capability.IdempotencyWindow(), 0)
	}
	return 0
}

// Idempotent reports whether p deduplicates sends at all; see
// IdempotencyWindow for how long.
func Idempotent(p Provider) bool { return IdempotencyWindow(p) > 0 }

// ValidateHeaders rejects values that could inject extra headers or recipients.
func (m Outbound) ValidateHeaders() error {
	values := []string{m.From.Name, m.From.Address, m.To.Name, m.To.Address, m.Subject}
	if m.ReplyTo != nil {
		values = append(values, m.ReplyTo.Name, m.ReplyTo.Address)
	}
	for name, value := range m.Headers {
		if name == "" || strings.ContainsAny(name, ": \t") {
			return errors.New("invalid email headers")
		}
		values = append(values, name, value)
	}
	for _, value := range values {
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("invalid email headers")
		}
	}
	return nil
}

// Disabled refuses every message. It is the default until a provider is set up.
type Disabled struct{}

func (Disabled) Name() string     { return "disabled" }
func (Disabled) CanDeliver() bool { return false }

func (Disabled) Send(context.Context, Outbound) (Result, error) { return Result{}, ErrDisabled }

// Log writes messages, including tokens, to the log instead of sending them.
// It is for local development only.
type Log struct{ Logger *slog.Logger }

func (Log) Name() string { return "log" }

func (l Log) Send(ctx context.Context, m Outbound) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	args := []any{"to", m.To.Address, "subject", m.Subject, "text", m.Text}
	if m.HTML != "" {
		args = append(args, "html", m.HTML)
	}
	logger.InfoContext(ctx, "development email", args...)
	return Result{}, nil
}
