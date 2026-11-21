// Package outbox is a persistent mail queue. Deliver writes a row, optionally
// inside the caller's transaction, so an email exists exactly when the business
// change that triggered it commits. Workers claim due rows with a lease, send
// them through an email.Provider and record the outcome, retrying according to
// the provider's error class. Several processes can share one database.
//
// Bodies are encrypted (secretbox, bound to the row ID), because they usually
// contain one-time tokens.
//
//	queued  ─claim→ sending ─ok→ sent
//	failed  ─claim→ sending ─retryable→ failed (later) / dead (attempts used up)
//	                        ─permanent→ dead
//	                        ─ambiguous, not safe to resend→ unknown ─webhook→ sent
//	                                                                ─timeout→ dead
//
// A sending row whose lease ran out (the worker crashed) is claimed again by
// any worker; that attempt's outcome is unknown, so it is treated like an
// ambiguous failure.
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xnqOS/go-starter-kit/kit/database"
	"github.com/xnqOS/go-starter-kit/kit/email"
	"github.com/xnqOS/go-starter-kit/kit/secretbox"
)

const (
	statusQueued  = "queued"
	statusSending = "sending"
	statusSent    = "sent"
	statusUnknown = "unknown"
	statusFailed  = "failed"
	statusDead    = "dead"
)

var (
	// ErrQueueFull means a backlog limit was reached; the caller may answer
	// 503 or try later. Rows waiting for a retry do not count, so a provider
	// outage does not block new mail.
	ErrQueueFull = errors.New("email queue full")
	// ErrQueueClosed is returned after Close.
	ErrQueueClosed = errors.New("email queue closed")
	// ErrInvalidMessage means Deliver was given a message that can never be
	// sent (no recipient, header injection, bad receipt).
	ErrInvalidMessage = errors.New("invalid email message")
)

// Receipt names a business row that records delivery. When the provider
// accepts the email, Table.sent_at is set for row ID; when it finally fails,
// Table.delivery_failed_at. The update runs in the same transaction as the
// outbox status change, so the two never disagree. The table must have both
// nullable timestamp columns.
type Receipt struct {
	Table string
	ID    string
}

var receiptTable = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Message is a rendered email to queue.
type Message struct {
	// Kind is a short label such as "password_reset", kept for logs and for
	// Options.ResendSafe. It is not sent.
	Kind     string
	Outbound email.Outbound
	// ExpiresAt, when set, is when the content stops being useful (a token's
	// expiry). Unsent mail is dropped after it and the stored body erased.
	ExpiresAt time.Time
	// Receipt, when set, is updated once delivery finished.
	Receipt *Receipt
	// Provider names the registry provider that sends it; "" records the
	// current default, so changing the default later does not move mail
	// that is already queued.
	Provider string
}

// Suppressions is checked before every attempt, so an address that bounced,
// complained or unsubscribed while its mail waited is not mailed.
type Suppressions interface {
	Suppressed(ctx context.Context, db *gorm.DB, recipient string) (bool, error)
}

// Options tune the outbox; zero values take the defaults.
type Options struct {
	Suppressions Suppressions
	// ResendSafe reports whether mail of a kind may be sent again after an
	// ambiguous failure (the provider may already have delivered it). Codes
	// and links are usually safe; receipts and notices usually are not.
	// Nil means never; providers that deduplicate by IdempotencyKey
	// (email.Idempotent) are always retried.
	ResendSafe func(kind string) bool
	// Workers is the number of sending goroutines (default 2).
	Workers int
	// PollInterval is how often idle workers look for due rows (default 1s).
	PollInterval time.Duration
	// Lease is how long a claimed row belongs to its worker (default 60s). It
	// must exceed SendTimeout plus the time to record the result.
	Lease time.Duration
	// MaxAttempts before a retryable failure becomes dead (default 6).
	MaxAttempts int
	// Backoff[i] is the wait after attempt i+1 failed; the last value
	// repeats (default 30s, 2m, 10m, 1h).
	Backoff []time.Duration
	// UnknownTimeout is how long an unknown row waits for a webhook before it
	// is marked dead (default 24h).
	UnknownTimeout time.Duration
	// Retention is how long finished (sent or dead) rows are kept (default 30 days).
	Retention time.Duration
	// SendTimeout bounds one provider call (default email.SendTimeout).
	SendTimeout time.Duration
	// Backlog caps rows waiting for their first send or being sent
	// (default 1000).
	Backlog int
	// PerRecipient caps those rows per address, so one abused form cannot
	// flood one inbox (default 5).
	PerRecipient int
}

func (o Options) withDefaults() Options {
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.Lease <= 0 {
		o.Lease = 60 * time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 6
	}
	if len(o.Backoff) == 0 {
		o.Backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour}
	}
	if o.UnknownTimeout <= 0 {
		o.UnknownTimeout = 24 * time.Hour
	}
	if o.Retention <= 0 {
		o.Retention = 30 * 24 * time.Hour
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = email.SendTimeout
	}
	if o.Backlog <= 0 {
		o.Backlog = 1000
	}
	if o.PerRecipient <= 0 {
		o.PerRecipient = 5
	}
	if o.ResendSafe == nil {
		o.ResendSafe = func(string) bool { return false }
	}
	return o
}

const (
	finishTimeout       = 5 * time.Second
	janitorInterval     = time.Minute
	janitorBatch        = 100
	claimCandidateBatch = 10
	// admissionLockClass is the PostgreSQL advisory lock key ("mail").
	admissionLockClass = 1835100524
)

type outboxRow struct {
	ID                string `gorm:"primaryKey"`
	IdempotencyKey    string
	Kind              string
	Recipient         string
	Payload           *string
	Status            string
	Attempts          int
	NextAttemptAt     time.Time
	ClaimToken        string
	LeaseExpiresAt    *time.Time
	Provider          string
	ProviderMessageID string
	LastErrorCode     string
	ReceiptTable      string
	ReceiptID         string
	ExpiresAt         *time.Time
	// MaybeAcceptedAt is when the earliest attempt that the provider may
	// have accepted was claimed (an ambiguous failure, or a worker that lost
	// its lease). A provider's deduplication window counts from there.
	MaybeAcceptedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	SentAt          *time.Time
}

func (outboxRow) TableName() string { return "email_outbox" }

// Outbox is the queue. Create it with New, call Start once, and Close on
// shutdown.
type Outbox struct {
	db           *gorm.DB
	providers    *Registry
	box          *secretbox.Box
	logger       *slog.Logger
	now          func() time.Time
	opts         Options
	suppressions Suppressions

	wake     chan struct{}
	sendCtx  context.Context
	cancel   context.CancelFunc
	stopping chan struct{}
	workers  sync.WaitGroup
	start    sync.Once
	stop     sync.Once
}

// New builds an outbox that sends with p (more providers can be added to
// Providers()). key must be 32 bytes; use secretbox.ParseKey for a base64
// environment value. Every process sharing the database needs the same key.
func New(db *gorm.DB, p email.Provider, key []byte, logger *slog.Logger, opts Options) (*Outbox, error) {
	if db == nil {
		return nil, errors.New("email outbox needs a database")
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, fmt.Errorf("email outbox key: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Outbox{
		db: db, providers: NewRegistry(p), box: box, logger: logger,
		now: func() time.Time { return time.Now().UTC() }, opts: opts, suppressions: opts.Suppressions,
		wake: make(chan struct{}, 1), sendCtx: ctx, cancel: cancel, stopping: make(chan struct{}),
	}, nil
}

// Providers is the registry the outbox sends with.
func (o *Outbox) Providers() *Registry { return o.providers }

// CanDeliver reports whether the default provider actually delivers mail
// (false for email.Disabled).
func (o *Outbox) CanDeliver() bool {
	if o == nil {
		return false
	}
	p, ok := o.providers.Get("")
	return ok && email.CanDeliver(p)
}

// Deliver queues m. With a non-nil tx the row is written inside it (in a
// savepoint, so an error leaves tx usable) and only exists if tx commits.
// A repeated Outbound.IdempotencyKey is accepted without queueing again.
func (o *Outbox) Deliver(ctx context.Context, tx *gorm.DB, m Message) error {
	if o == nil {
		return ErrQueueClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-o.stopping:
		return ErrQueueClosed
	default:
	}
	if err := validate(m); err != nil {
		return err
	}
	now := o.now()
	row := outboxRow{
		ID: newID(), IdempotencyKey: m.Outbound.IdempotencyKey, Kind: m.Kind,
		Recipient: normalizeAddress(m.Outbound.To.Address), Status: statusQueued,
		Provider: m.Provider, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if row.IdempotencyKey == "" {
		row.IdempotencyKey = row.ID
	}
	if row.Provider == "" {
		row.Provider = o.providers.Default()
	}
	if !m.ExpiresAt.IsZero() {
		expires := m.ExpiresAt.UTC()
		row.ExpiresAt = &expires
	}
	if m.Receipt != nil {
		row.ReceiptTable, row.ReceiptID = m.Receipt.Table, m.Receipt.ID
	}
	payload, err := o.seal(row.ID, m.Outbound)
	if err != nil {
		return err
	}
	row.Payload = &payload
	insert := func(tx *gorm.DB) error {
		// Admission is serialized (PostgreSQL: an advisory lock held until
		// the caller commits; SQLite: one writer at a time), so concurrent
		// enqueues cannot all pass the same backlog limit.
		if err := lockAdmission(tx); err != nil {
			return err
		}
		var existing int64
		if err := tx.Model(&outboxRow{}).Where("idempotency_key = ?", row.IdempotencyKey).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return nil
		}
		if err := o.admit(tx, row); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true}).Create(&row).Error
	}
	if tx != nil {
		err = database.WithSavepoint(tx, insert)
	} else {
		err = o.db.WithContext(ctx).Transaction(insert)
	}
	if err == nil {
		o.nudge()
	} else if errors.Is(err, ErrQueueFull) {
		o.logger.WarnContext(ctx, "outbound email queue rejected", "kind", row.Kind)
	}
	return err
}

func validate(m Message) error {
	if strings.TrimSpace(m.Outbound.To.Address) == "" {
		return fmt.Errorf("%w: no recipient", ErrInvalidMessage)
	}
	if err := m.Outbound.ValidateHeaders(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if r := m.Receipt; r != nil && (!receiptTable.MatchString(r.Table) || r.ID == "") {
		return fmt.Errorf("%w: receipt needs a table name and a row ID", ErrInvalidMessage)
	}
	return nil
}

// lockAdmission takes the admission lock until tx ends. Callers enqueue as
// the last write of their transaction, so holding it until commit only queues
// other enqueues behind short commits. SQLite needs no lock: connections from
// database.Open begin IMMEDIATE, so writers already run one at a time.
func lockAdmission(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(?, 0)", admissionLockClass).Error
}

func (o *Outbox) admit(tx *gorm.DB, row outboxRow) error {
	active := func() *gorm.DB {
		return tx.Model(&outboxRow{}).Where("status IN ?", []string{statusQueued, statusSending})
	}
	limits := []struct {
		query *gorm.DB
		max   int
	}{
		{active(), o.opts.Backlog},
		{active().Where("recipient = ?", row.Recipient), o.opts.PerRecipient},
	}
	for _, l := range limits {
		var n int64
		if err := l.query.Count(&n).Error; err != nil {
			return err
		}
		if n >= int64(l.max) {
			return ErrQueueFull
		}
	}
	return nil
}

func (o *Outbox) nudge() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Start launches the workers and the janitor. Close stops them.
func (o *Outbox) Start() {
	o.start.Do(func() {
		for i := 0; i < o.opts.Workers; i++ {
			o.workers.Add(1)
			go o.work()
		}
		o.workers.Add(1)
		go o.janitor()
	})
}

// Close stops claiming new rows and waits for sends in progress. When ctx
// ends first, those sends are cancelled; rows that could not be finished are
// claimed again once their lease expires.
func (o *Outbox) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.stop.Do(func() { close(o.stopping) })
	done := make(chan struct{})
	go func() { o.workers.Wait(); close(done) }()
	select {
	case <-done:
		o.cancel()
		return nil
	case <-ctx.Done():
		o.cancel()
		<-done
		return ctx.Err()
	}
}

func (o *Outbox) work() {
	defer o.workers.Done()
	for {
		select {
		case <-o.stopping:
			return
		default:
		}
		worked, err := o.process(o.sendCtx)
		if err != nil && o.sendCtx.Err() == nil {
			o.logger.Error("email outbox claim failed", "error", err)
		}
		if worked {
			continue
		}
		timer := time.NewTimer(o.opts.PollInterval)
		select {
		case <-o.stopping:
			timer.Stop()
			return
		case <-o.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (o *Outbox) janitor() {
	defer o.workers.Done()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		if err := o.sweep(o.sendCtx); err != nil && o.sendCtx.Err() == nil {
			o.logger.Error("email outbox sweep failed", "error", err)
		}
		select {
		case <-o.stopping:
			return
		case <-ticker.C:
		}
	}
}

// process claims and sends at most one row. It reports whether it found one.
func (o *Outbox) process(ctx context.Context) (bool, error) {
	row, reclaimed, err := o.claim(ctx)
	if err != nil || row == nil {
		return false, err
	}
	o.send(ctx, row, reclaimed)
	return true, nil
}

const dueCondition = "((status IN ('queued', 'failed') AND next_attempt_at <= ?) OR (status = 'sending' AND lease_expires_at < ?))"

// claim takes one due row with a conditional UPDATE, so two workers (in any
// process) never hold the same row. reclaimed is true when the row was left
// in sending by a worker whose lease expired.
func (o *Outbox) claim(ctx context.Context) (*outboxRow, bool, error) {
	var claimed *outboxRow
	reclaimed := false
	err := o.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := o.now()
		q := tx.Model(&outboxRow{}).Select("id", "status", "updated_at").Where(dueCondition, now, now)
		if tx.Dialector.Name() == "postgres" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		var candidates []outboxRow
		if err := q.Order("next_attempt_at, id").Limit(claimCandidateBatch).Find(&candidates).Error; err != nil {
			return err
		}
		for _, candidate := range candidates {
			updates := map[string]any{
				"status": statusSending, "claim_token": newID(), "lease_expires_at": now.Add(o.opts.Lease),
				"attempts": gorm.Expr("attempts + 1"), "updated_at": now,
			}
			if candidate.Status == statusSending {
				// The worker whose lease ran out may have sent it. Its claim
				// set updated_at, and nothing else touches a sending row.
				updates["maybe_accepted_at"] = gorm.Expr("COALESCE(maybe_accepted_at, ?)", candidate.UpdatedAt)
			}
			result := tx.Model(&outboxRow{}).
				Where("id = ? AND status = ?", candidate.ID, candidate.Status).Where(dueCondition, now, now).
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue // another worker got it first
			}
			var row outboxRow
			if err := tx.Where("id = ?", candidate.ID).Take(&row).Error; err != nil {
				return err
			}
			claimed, reclaimed = &row, candidate.Status == statusSending
			return nil
		}
		return nil
	})
	return claimed, reclaimed, err
}

// outcome is the result of one attempt.
type outcome struct {
	status    string
	code      string
	messageID string
	nextAt    time.Time
	keepBody  bool
	receipt   string // "sent_at", "delivery_failed_at" or ""
	// maybeAccepted is the claim time of an attempt the provider may have
	// accepted; it is kept unless an earlier one is already recorded.
	maybeAccepted *time.Time
}

func (o *Outbox) send(ctx context.Context, row *outboxRow, reclaimed bool) {
	now := o.now()
	switch {
	case row.ExpiresAt != nil && !now.Before(*row.ExpiresAt):
		o.finish(row, outcome{status: statusDead, code: "expired", receipt: "delivery_failed_at"}, nil)
		return
	case row.Payload == nil:
		o.finish(row, outcome{status: statusDead, code: "no_payload", receipt: "delivery_failed_at"}, nil)
		return
	case row.Attempts > o.opts.MaxAttempts:
		// Attempts count claims, so a message whose worker keeps crashing
		// (and never records a result) also ends here.
		o.finish(row, outcome{status: statusDead, code: "attempts_exhausted", receipt: "delivery_failed_at"}, nil)
		return
	}
	out, err := o.open(row.ID, *row.Payload)
	if err != nil {
		// Wrong or rotated key; retrying cannot help.
		o.logger.Error("email outbox body cannot be decrypted; check the outbox key", "kind", row.Kind)
		o.finish(row, outcome{status: statusDead, code: "undecryptable", receipt: "delivery_failed_at"}, nil)
		return
	}
	p, ok := o.providers.Get(row.Provider)
	if row.MaybeAcceptedAt != nil && !o.mayResend(p, ok, row.Kind, *row.MaybeAcceptedAt, now) {
		// An earlier attempt may have been delivered (a worker crashed, or
		// the provider's reply was lost), and sending again now could
		// deliver twice. This comes before every check that can end in a
		// retry, so the "maybe sent" state is never turned into an ordinary
		// retry.
		code := "resend_unsafe"
		if reclaimed {
			code = "lease_expired"
		}
		o.finish(row, outcome{status: statusUnknown, code: code}, nil)
		return
	}
	// An ambiguous failure of this attempt may be retried only if the retry
	// cannot deliver twice; send checks the provider's window again then.
	resendSafe := o.opts.ResendSafe(row.Kind) || (ok && email.Idempotent(p))
	if o.suppressions != nil {
		blocked, err := o.suppressions.Suppressed(ctx, o.db.WithContext(ctx), row.Recipient)
		if err != nil {
			o.finish(row, o.outcomeOf(row, email.Result{}, email.Failed(email.Retryable, "suppression_check", err), true), nil)
			return
		}
		if blocked {
			o.finish(row, outcome{status: statusDead, code: "suppressed", receipt: "delivery_failed_at"}, nil)
			return
		}
	}
	if !ok {
		// The provider was removed from the configuration; it may come back.
		o.finish(row, o.outcomeOf(row, email.Result{}, email.Failed(email.Retryable, "provider_unavailable", fmt.Errorf("provider %q is not configured", row.Provider)), true), nil)
		return
	}
	// Every attempt carries the same key and outbox_id tag, so a provider
	// that deduplicates drops repeats, and webhooks find this row.
	out.IdempotencyKey = row.IdempotencyKey
	tags := make(map[string]string, len(out.Tags)+1)
	for k, v := range out.Tags {
		tags[k] = v
	}
	tags["outbox_id"] = row.ID
	out.Tags = tags
	// The checks above (suppressions, decrypting) take time, so the window
	// is checked again with the time the request actually starts.
	if row.MaybeAcceptedAt != nil && !o.mayResend(p, ok, row.Kind, *row.MaybeAcceptedAt, o.now()) {
		o.finish(row, outcome{status: statusUnknown, code: "resend_unsafe"}, nil)
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, o.opts.SendTimeout)
	result, err := p.Send(sendCtx, out)
	cancel()
	o.finish(row, o.outcomeOf(row, result, err, resendSafe), err)
}

// mayResend reports whether a message that the provider may already have
// accepted (at the earliest when an attempt was claimed at maybeAccepted) can
// be sent again at now without risking a second delivery: the kind is
// resend-safe, or the provider deduplicates by IdempotencyKey and the new
// request reaches it (within SendTimeout of now) inside its window.
func (o *Outbox) mayResend(p email.Provider, ok bool, kind string, maybeAccepted, now time.Time) bool {
	if o.opts.ResendSafe(kind) {
		return true
	}
	if !ok {
		return false
	}
	window := email.IdempotencyWindow(p)
	return window > 0 && now.Add(o.opts.SendTimeout).Sub(maybeAccepted) < window
}

func (o *Outbox) outcomeOf(row *outboxRow, result email.Result, err error, resendSafe bool) outcome {
	if err == nil {
		return outcome{status: statusSent, messageID: result.ProviderMessageID, receipt: "sent_at"}
	}
	code := email.Code(err)
	if code == "" {
		code = "error"
	}
	class := email.Classify(err)
	var maybeAccepted *time.Time
	if class == email.Ambiguous {
		claimed := row.UpdatedAt // set by claim
		maybeAccepted = &claimed
	}
	switch {
	case class == email.Permanent:
		return outcome{status: statusDead, code: code, receipt: "delivery_failed_at"}
	case class == email.Ambiguous && !resendSafe:
		return outcome{status: statusUnknown, code: code, maybeAccepted: maybeAccepted}
	case row.Attempts >= o.opts.MaxAttempts:
		return outcome{status: statusDead, code: code, receipt: "delivery_failed_at"}
	}
	i := min(max(row.Attempts-1, 0), len(o.opts.Backoff)-1)
	return outcome{status: statusFailed, code: code, nextAt: o.now().Add(o.opts.Backoff[i]), keepBody: true, maybeAccepted: maybeAccepted}
}

// finish records an attempt's outcome, but only while this worker still owns
// the row (claim_token matches). A worker whose lease expired and whose row was
// claimed again must not overwrite the newer result.
func (o *Outbox) finish(row *outboxRow, out outcome, sendErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	stale := false
	err := o.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ok, err := applyOutcome(tx, o.now(), row.ID, "claim_token = ?", row.ClaimToken, out, row.ReceiptTable, row.ReceiptID)
		stale = !ok
		return err
	})
	args := []any{"kind", row.Kind, "status", out.status, "attempts", row.Attempts}
	switch {
	case err != nil:
		o.logger.Error("email outbox result not recorded", append(args, "error", err)...)
	case stale:
		o.logger.Warn("email outbox result discarded; the row was claimed again or settled by a webhook", args...)
	case sendErr != nil:
		o.logger.Warn("outbound email delivery failed", append(args, "class", email.Classify(sendErr).String(), "code", out.code)...)
	case out.status != statusSent:
		o.logger.Warn("outbound email not sent", append(args, "code", out.code)...)
	}
}

// applyOutcome moves one row (matching id and the extra condition) to out,
// and writes the business receipt in the same transaction. It reports whether
// the row matched.
func applyOutcome(tx *gorm.DB, now time.Time, id, condition string, arg any, out outcome, table, receiptID string) (bool, error) {
	updates := map[string]any{
		"status": out.status, "last_error_code": out.code, "claim_token": "", "lease_expires_at": nil, "updated_at": now,
	}
	if !out.keepBody {
		updates["payload"] = nil
	}
	if out.status == statusFailed {
		updates["next_attempt_at"] = out.nextAt
	}
	if out.maybeAccepted != nil {
		updates["maybe_accepted_at"] = gorm.Expr("COALESCE(maybe_accepted_at, ?)", *out.maybeAccepted)
	}
	if out.status == statusSent {
		updates["sent_at"] = now
		updates["last_error_code"] = ""
		if out.messageID != "" {
			updates["provider_message_id"] = out.messageID
		}
	}
	result := tx.Model(&outboxRow{}).Where("id = ?", id).Where(condition, arg).Updates(updates)
	if result.Error != nil || result.RowsAffected != 1 {
		return false, result.Error
	}
	if out.receipt == "" || table == "" {
		return true, nil
	}
	if !receiptTable.MatchString(table) {
		return true, fmt.Errorf("invalid email receipt table %q", table)
	}
	return true, tx.Table(table).Where("id = ?", receiptID).Update(out.receipt, now).Error
}

// sweep handles rows no worker will touch: expired unsent mail, bodies past
// their expiry, unknown rows nobody confirmed, and old finished rows.
func (o *Outbox) sweep(ctx context.Context) error {
	now := o.now()
	db := o.db.WithContext(ctx)
	resolve := func(where string, args []any, out outcome, log func(outboxRow)) error {
		var rows []outboxRow
		if err := db.Select("id", "status", "kind", "receipt_table", "receipt_id").Where(where, args...).Limit(janitorBatch).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			err := db.Transaction(func(tx *gorm.DB) error {
				ok, err := applyOutcome(tx, now, row.ID, "status = ?", row.Status, out, row.ReceiptTable, row.ReceiptID)
				if ok && err == nil && log != nil {
					log(row)
				}
				return err
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := resolve("status IN ? AND expires_at IS NOT NULL AND expires_at <= ?", []any{[]string{statusQueued, statusFailed}, now},
		outcome{status: statusDead, code: "expired", receipt: "delivery_failed_at"}, nil); err != nil {
		return err
	}
	if err := resolve("status = ? AND updated_at <= ?", []any{statusUnknown, now.Add(-o.opts.UnknownTimeout)},
		outcome{status: statusDead, code: "unknown_timeout", receipt: "delivery_failed_at"},
		func(row outboxRow) {
			o.logger.Error("email delivery outcome still unknown; marked dead", "kind", row.Kind)
		}); err != nil {
		return err
	}
	if err := db.Model(&outboxRow{}).Where("payload IS NOT NULL AND status <> ? AND expires_at IS NOT NULL AND expires_at <= ?", statusSending, now).
		Update("payload", nil).Error; err != nil {
		return err
	}
	return db.Where("status IN ? AND updated_at <= ?", []string{statusSent, statusDead}, now.Add(-o.opts.Retention)).Delete(&outboxRow{}).Error
}

// Resolve settles the row an event refers to, in its own transaction. Call
// it from the webhook handler for every event a provider's
// email.WebhookHandler returns.
func (o *Outbox) Resolve(ctx context.Context, event email.Event) error {
	return o.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return resolveEvent(tx, o.now(), event)
	})
}

// ResolveEvent is Resolve inside the caller's transaction, for handlers that
// also record the event (deduplication, suppressions) in the same commit.
func ResolveEvent(tx *gorm.DB, event email.Event) error {
	return resolveEvent(tx, time.Now().UTC(), event)
}

// resolveEvent settles a row once the provider reports on the message: any
// delivery event proves the provider took it. The row may still be in sending
// (the webhook beat the provider's response or a crashed worker's lease) or
// failed (waiting to retry after a timeout); settling those too means the
// event is not lost and the email is not sent again. The worker that still
// holds a sending row then fails its claim_token check. Finished rows (sent,
// dead) are left alone.
func resolveEvent(tx *gorm.DB, now time.Time, event email.Event) error {
	var out outcome
	switch event.Type {
	case email.EventDelivered, email.EventBounced, email.EventComplained, email.EventOpened, email.EventUnsubscribed:
		out = outcome{status: statusSent, messageID: event.ProviderMessageID, receipt: "sent_at"}
	case email.EventDropped:
		out = outcome{status: statusDead, code: "dropped", receipt: "delivery_failed_at"}
	default:
		return nil
	}
	pending := []string{statusUnknown, statusSending, statusFailed}
	q := tx.Model(&outboxRow{}).Select("id", "receipt_table", "receipt_id").Where("status IN ?", pending)
	switch {
	case event.Tags["outbox_id"] != "":
		q = q.Where("id = ?", event.Tags["outbox_id"])
	case event.ProviderMessageID != "":
		q = q.Where("provider_message_id = ?", event.ProviderMessageID)
	default:
		return nil
	}
	var rows []outboxRow
	if err := q.Limit(1).Find(&rows).Error; err != nil || len(rows) == 0 {
		return err
	}
	_, err := applyOutcome(tx, now, rows[0].ID, "status IN ?", pending, out, rows[0].ReceiptTable, rows[0].ReceiptID)
	return err
}

func (o *Outbox) seal(id string, out email.Outbound) (string, error) {
	plain, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	// The row ID is authenticated data, so a body cannot be moved to another row.
	return o.box.Seal(id, plain)
}

func (o *Outbox) open(id, payload string) (email.Outbound, error) {
	var out email.Outbound
	plain, err := o.box.Open(id, payload)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(plain, &out)
}

// normalizeAddress is the form recipients are counted and suppressed by.
func normalizeAddress(raw string) string {
	if parsed, err := mail.ParseAddress(raw); err == nil {
		raw = parsed.Address
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func newID() string { return rand.Text() }
