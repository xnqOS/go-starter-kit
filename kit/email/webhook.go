package email

import (
	"errors"
	"net/http"
	"time"
)

// EventType is a provider-neutral delivery event.
type EventType string

const (
	EventDelivered    EventType = "delivered"
	EventBounced      EventType = "bounced"
	EventComplained   EventType = "complained"
	EventUnsubscribed EventType = "unsubscribed"
	EventOpened       EventType = "opened"
	// EventDropped: the provider refused to send (for example its own
	// suppression list). Recorded, but it does not add to ours.
	EventDropped EventType = "dropped"
)

// Event is one webhook event converted to our terms.
type Event struct {
	// ID is the provider's event ID, used to ignore redelivered webhooks.
	ID                string
	Type              EventType
	ProviderMessageID string
	Recipient         string
	OccurredAt        time.Time
	// HardBounce is true for permanent bounces; only those suppress the address.
	HardBounce bool
	// Tags are the tags we sent (for example kind, outbox_id), when echoed back.
	Tags map[string]string
}

// WebhookHandler is implemented by adapters that receive delivery webhooks.
type WebhookHandler interface {
	Name() string
	// Parse checks the signature and converts the body. Events the adapter
	// does not care about are skipped, so the result may be empty.
	Parse(r *http.Request, body []byte) ([]Event, error)
}

// ErrBadSignature means a webhook was not signed by the provider.
var ErrBadSignature = errors.New("invalid webhook signature")

// WebhookTolerance is how far a signed timestamp may be from now. It limits
// replaying a captured webhook.
const WebhookTolerance = 5 * time.Minute

// FreshTimestamp reports whether ts is within WebhookTolerance of now.
func FreshTimestamp(ts, now time.Time) bool {
	d := now.Sub(ts)
	return d <= WebhookTolerance && d >= -WebhookTolerance
}
