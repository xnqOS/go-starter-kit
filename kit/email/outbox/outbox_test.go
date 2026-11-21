package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/xnqOS/go-starter-kit/kit/database"
	"github.com/xnqOS/go-starter-kit/kit/email"
	"github.com/xnqOS/go-starter-kit/kit/email/resend"
)

// fakeProvider records every attempt and answers with the next scripted error.
type fakeProvider struct {
	name string
	// window, when set, makes the fake deduplicate by IdempotencyKey for
	// that long (as far as the outbox can tell).
	window   time.Duration
	mu       sync.Mutex
	attempts []email.Outbound
	script   []error
}

func (p *fakeProvider) Name() string {
	if p.name == "" {
		return "fake"
	}
	return p.name
}

func (p *fakeProvider) Send(ctx context.Context, m email.Outbound) (email.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts = append(p.attempts, m)
	if len(p.script) > 0 {
		err := p.script[0]
		p.script = p.script[1:]
		if err != nil {
			return email.Result{}, err
		}
	}
	return email.Result{ProviderMessageID: fmt.Sprintf("msg-%d", len(p.attempts))}, nil
}

func (p *fakeProvider) IdempotencyWindow() time.Duration { return p.window }

func (p *fakeProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.attempts) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }
func (c *clock) set(t time.Time)     { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

func quietLogger() *slog.Logger   { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func testKey() []byte             { return []byte("0123456789abcdef0123456789abcdef") }
func ambiguous(code string) error { return email.Failed(email.Ambiguous, code, errors.New(code)) }
func retryable(code string) error { return email.Failed(email.Retryable, code, errors.New(code)) }
func permanent(code string) error { return email.Failed(email.Permanent, code, errors.New(code)) }
func receiptOf(id string) *Receipt {
	return &Receipt{Table: "test_receipts", ID: id}
}

// message is a password reset for to, carrying token in its body.
func message(to, token string) Message {
	return Message{Kind: "password_reset", Outbound: email.Outbound{
		From: mail.Address{Name: "Example", Address: "no-reply@example.com"}, To: mail.Address{Address: to},
		Subject: "Reset your password", Text: "Token: " + token + "\n",
	}}
}

// openTestDB opens SQLite, or PostgreSQL in a fresh schema when dialect is
// "postgres" and TEST_DATABASE_URL is set. The schema goes into the DSN so
// every pooled connection sees it (the concurrency tests need several).
func openTestDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "outbox.db")
	if dialect == "postgres" {
		dsn = os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("TEST_DATABASE_URL not set")
		}
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := database.Open(ctx, dialect, dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := "outbox_test_" + strings.ToLower(newID())
		if err := admin.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error; _ = database.Close(admin) })
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		dsn = parsed.String()
	}
	db, err := database.Open(ctx, dialect, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := db.Transaction(CreateTableV1); err != nil {
		t.Fatal(err)
	}
	return db
}

type fixture struct {
	db     *gorm.DB
	clock  *clock
	outbox *Outbox
}

func newFixture(t *testing.T, dialect string, p email.Provider, opts Options) *fixture {
	t.Helper()
	db := openTestDB(t, dialect)
	if err := db.Exec(`CREATE TABLE test_receipts (id TEXT PRIMARY KEY, sent_at TIMESTAMP, delivery_failed_at TIMESTAMP)`).Error; err != nil {
		t.Fatal(err)
	}
	if opts.SendTimeout == 0 {
		opts.SendTimeout = 300 * time.Millisecond
	}
	o, err := New(db, p, testKey(), quietLogger(), opts)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	o.now = c.now
	return &fixture{db: db, clock: c, outbox: o}
}

// queue sends a password reset to <id>@example.com inside a transaction that
// also creates the test_receipts row id.
func (f *fixture) queue(t *testing.T, id string, edit ...func(*Message)) {
	t.Helper()
	m := message(id+"@example.com", "secret-"+id)
	m.Receipt = receiptOf(id)
	for _, e := range edit {
		e(&m)
	}
	err := f.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO test_receipts (id) VALUES (?)`, id).Error; err != nil {
			return err
		}
		return f.outbox.Deliver(context.Background(), tx, m)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) row(t *testing.T, recipient string) outboxRow {
	t.Helper()
	var row outboxRow
	if err := f.db.Where("recipient = ?", recipient).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *fixture) receipt(t *testing.T, id string) (sent, failed bool) {
	t.Helper()
	var r struct {
		SentAt           *time.Time
		DeliveryFailedAt *time.Time
	}
	if err := f.db.Table("test_receipts").Where("id = ?", id).Take(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r.SentAt != nil, r.DeliveryFailedAt != nil
}

func (f *fixture) process(t *testing.T) bool {
	t.Helper()
	worked, err := f.outbox.process(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return worked
}

func (f *fixture) count(t *testing.T, where string, args ...any) int64 {
	t.Helper()
	var n int64
	q := f.db.Model(&outboxRow{})
	if where != "" {
		q = q.Where(where, args...)
	}
	if err := q.Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func forEachDialect(t *testing.T, fn func(t *testing.T, dialect string)) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) { fn(t, dialect) })
	}
}

func TestQueuesInTransactionEncryptsAndRecordsReceipt(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{}
		f := newFixture(t, dialect, p, Options{})
		ctx := context.Background()
		// Rolled back with the business change: no email.
		rollback := errors.New("rollback")
		err := f.db.Transaction(func(tx *gorm.DB) error {
			if err := f.outbox.Deliver(ctx, tx, message("gone@example.com", "x")); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		f.queue(t, "staff", func(m *Message) { m.Outbound.To.Address = " Staff@Example.com " })
		if n := f.count(t, ""); n != 1 {
			t.Fatalf("outbox rows = %d, want only the committed one", n)
		}
		row := f.row(t, "staff@example.com")
		if row.Status != statusQueued || row.Payload == nil || strings.Contains(*row.Payload, "secret-staff") || row.IdempotencyKey != row.ID || row.Provider != "fake" || row.Kind != "password_reset" {
			t.Fatalf("queued row: %+v", row)
		}
		if !f.process(t) || f.process(t) {
			t.Fatal("expected exactly one row to send")
		}
		got := p.attempts[0]
		if got.Text != "Token: secret-staff\n" || got.IdempotencyKey != row.ID || got.Tags["outbox_id"] != row.ID || got.From.Address != "no-reply@example.com" {
			t.Fatalf("sent: %+v", got)
		}
		row = f.row(t, "staff@example.com")
		if row.Status != statusSent || row.Payload != nil || row.ProviderMessageID != "msg-1" || row.SentAt == nil || row.Attempts != 1 {
			t.Fatalf("sent row: %+v", row)
		}
		if sent, failed := f.receipt(t, "staff"); !sent || failed {
			t.Fatal("receipt not recorded")
		}
		// The same idempotency key is accepted but not queued twice.
		dup := message("dup@example.com", "d")
		dup.Outbound.IdempotencyKey = "signup-42"
		for range 2 {
			if err := f.outbox.Deliver(ctx, nil, dup); err != nil {
				t.Fatal(err)
			}
		}
		if n := f.count(t, "idempotency_key = ?", "signup-42"); n != 1 {
			t.Fatalf("duplicate idempotency key queued %d rows", n)
		}
		// Messages that can never be sent are refused up front.
		for name, edit := range map[string]func(*Message){
			"no recipient":     func(m *Message) { m.Outbound.To.Address = "" },
			"header injection": func(m *Message) { m.Outbound.Subject = "Hi\r\nBcc: x@example.com" },
			"bad receipt":      func(m *Message) { m.Receipt = &Receipt{Table: "x; DROP TABLE y", ID: "1"} },
		} {
			m := message("bad@example.com", "b")
			edit(&m)
			if err := f.outbox.Deliver(ctx, nil, m); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
}

func TestRetriesWithBackoffAndGivesUp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{script: []error{retryable("http_429"), retryable("http_429"), permanent("http_422")}}
		f := newFixture(t, dialect, p, Options{})
		f.queue(t, "a")
		f.process(t)
		row := f.row(t, "a@example.com")
		if row.Status != statusFailed || row.LastErrorCode != "http_429" || row.Payload == nil || !row.NextAttemptAt.Equal(f.clock.now().Add(30*time.Second)) {
			t.Fatalf("after retryable failure: %+v", row)
		}
		if f.process(t) {
			t.Fatal("retried before backoff elapsed")
		}
		f.clock.add(30 * time.Second)
		f.process(t)
		if row = f.row(t, "a@example.com"); !row.NextAttemptAt.Equal(f.clock.now().Add(2 * time.Minute)) {
			t.Fatalf("second backoff: %v", row.NextAttemptAt)
		}
		f.clock.add(2 * time.Minute)
		f.process(t)
		row = f.row(t, "a@example.com")
		if row.Status != statusDead || row.LastErrorCode != "http_422" || row.Payload != nil {
			t.Fatalf("permanent failure: %+v", row)
		}
		if sent, failed := f.receipt(t, "a"); sent || !failed {
			t.Fatal("failure receipt not recorded")
		}
		keys := map[string]bool{}
		for _, attempt := range p.attempts {
			keys[attempt.IdempotencyKey] = true
		}
		if len(p.attempts) != 3 || len(keys) != 1 {
			t.Fatalf("attempts=%d distinct idempotency keys=%d", len(p.attempts), len(keys))
		}

		// Retryable errors stop after MaxAttempts.
		p.script = []error{retryable("x"), retryable("x"), retryable("x"), retryable("x"), retryable("x"), retryable("x")}
		f.queue(t, "b")
		for i := 0; i < 6; i++ {
			f.clock.add(2 * time.Hour)
			if !f.process(t) {
				t.Fatalf("attempt %d not claimed", i+1)
			}
		}
		if row := f.row(t, "b@example.com"); row.Status != statusDead || row.Attempts != 6 {
			t.Fatalf("after max attempts: %+v", row)
		}
	})
}

func TestCrashedWorkerLeaseAndStaleCompletion(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{}
		f := newFixture(t, dialect, p, Options{ResendSafe: func(string) bool { return true }})
		f.queue(t, "a")
		// Worker 1 claims and "crashes": it never records a result.
		first, reclaimed, err := f.outbox.claim(context.Background())
		if err != nil || first == nil || reclaimed {
			t.Fatalf("first claim: %v %v %v", first, reclaimed, err)
		}
		f.clock.add(59 * time.Second)
		if f.process(t) {
			t.Fatal("another worker took a row whose lease is still valid")
		}
		f.clock.add(2 * time.Second)
		if !f.process(t) {
			t.Fatal("expired lease was not reclaimed")
		}
		row := f.row(t, "a@example.com")
		if row.Status != statusSent || row.Attempts != 2 || p.count() != 1 {
			t.Fatalf("reclaimed send: %+v attempts=%d", row, p.count())
		}
		// Worker 1 wakes up late and reports a failure: ignored.
		f.outbox.finish(first, outcome{status: statusFailed, code: "late", nextAt: f.clock.now(), keepBody: true, receipt: "delivery_failed_at"}, nil)
		row = f.row(t, "a@example.com")
		if row.Status != statusSent || row.LastErrorCode != "" {
			t.Fatalf("stale worker overwrote the result: %+v", row)
		}
		if sent, failed := f.receipt(t, "a"); !sent || failed {
			t.Fatal("stale worker changed the receipt")
		}

		// A message whose worker always crashes ends up dead.
		f.queue(t, "b")
		for i := 0; i < 6; i++ {
			if row, _, err := f.outbox.claim(context.Background()); err != nil || row == nil {
				t.Fatalf("crash loop claim %d: %v", i+1, err)
			}
			f.clock.add(61 * time.Second)
		}
		f.process(t)
		if row := f.row(t, "b@example.com"); row.Status != statusDead || row.LastErrorCode != "attempts_exhausted" {
			t.Fatalf("crash loop: %+v", row)
		}
		if _, failed := f.receipt(t, "b"); !failed {
			t.Fatal("crash loop receipt")
		}
	})
}

func TestAmbiguousWithoutIdempotency(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{script: []error{ambiguous("timeout_after_write"), nil, ambiguous("timeout_after_write")}}
		f := newFixture(t, dialect, p, Options{ResendSafe: func(kind string) bool { return kind == "password_reset" }})
		// A resend-safe kind is retried.
		f.queue(t, "code")
		f.process(t)
		if row := f.row(t, "code@example.com"); row.Status != statusFailed {
			t.Fatalf("resend-safe kind: %+v", row)
		}
		f.clock.add(time.Minute)
		f.process(t)
		if row := f.row(t, "code@example.com"); row.Status != statusSent {
			t.Fatalf("retry: %+v", row)
		}

		// Any other kind waits for a webhook instead.
		f.queue(t, "order", func(m *Message) { m.Kind = "order_receipt" })
		f.process(t)
		row := f.row(t, "order@example.com")
		if row.Status != statusUnknown || row.Payload != nil {
			t.Fatalf("ambiguous, not resend-safe: %+v", row)
		}
		f.clock.add(time.Hour)
		if f.process(t) {
			t.Fatal("unknown row was resent")
		}
		if err := f.outbox.Resolve(context.Background(), email.Event{ID: "ev1", Type: email.EventDelivered, ProviderMessageID: "pm-9", Recipient: "order@example.com", Tags: map[string]string{"outbox_id": row.ID}}); err != nil {
			t.Fatal(err)
		}
		row = f.row(t, "order@example.com")
		if row.Status != statusSent || row.ProviderMessageID != "pm-9" {
			t.Fatalf("webhook did not settle the row: %+v", row)
		}
		if sent, _ := f.receipt(t, "order"); !sent {
			t.Fatal("webhook did not write the receipt")
		}

		// A reclaimed lease is "result unknown" too.
		f.queue(t, "crash", func(m *Message) { m.Kind = "order_receipt" })
		if r, _, _ := f.outbox.claim(context.Background()); r == nil {
			t.Fatal("claim")
		}
		f.clock.add(2 * time.Minute)
		f.process(t)
		if row := f.row(t, "crash@example.com"); row.Status != statusUnknown || row.LastErrorCode != "lease_expired" {
			t.Fatalf("reclaimed, not resend-safe: %+v", row)
		}
		// Nobody confirms it within 24 hours: dead.
		f.clock.add(25 * time.Hour)
		if err := f.outbox.sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if row := f.row(t, "crash@example.com"); row.Status != statusDead || row.LastErrorCode != "unknown_timeout" {
			t.Fatalf("unknown timeout: %+v", row)
		}
		if _, failed := f.receipt(t, "crash"); !failed {
			t.Fatal("unknown timeout receipt")
		}

		// ResolveEvent in the caller's transaction, matched by provider message ID.
		f.queue(t, "late", func(m *Message) { m.Kind = "order_receipt" })
		p.script = []error{ambiguous("x")}
		f.process(t)
		if err := f.db.Model(&outboxRow{}).Where("recipient = ?", "late@example.com").Update("provider_message_id", "pm-late").Error; err != nil {
			t.Fatal(err)
		}
		if err := f.db.Transaction(func(tx *gorm.DB) error {
			return ResolveEvent(tx, email.Event{Type: email.EventDropped, ProviderMessageID: "pm-late"})
		}); err != nil {
			t.Fatal(err)
		}
		if row := f.row(t, "late@example.com"); row.Status != statusDead || row.LastErrorCode != "dropped" {
			t.Fatalf("dropped event: %+v", row)
		}
	})
}

// resendFake accepts each Idempotency-Key once, like Resend, and hangs on the
// first request after accepting it, so the client times out.
type resendFake struct {
	mu        sync.Mutex
	delivered map[string]int
	requests  []string
}

func (s *resendFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	key := r.Header.Get("Idempotency-Key")
	s.mu.Lock()
	s.requests = append(s.requests, key)
	first := len(s.requests) == 1
	if s.delivered[key] == 0 {
		s.delivered[key]++
	}
	s.mu.Unlock()
	if first {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"id":"re_1"}`)
}

func TestAmbiguousWithIdempotentProviderDeliversOnce(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		fake := &resendFake{delivered: map[string]int{}}
		server := httptest.NewServer(fake)
		defer server.Close()
		p := resend.New(resend.Config{APIKey: "re_test"}).WithBaseURL(server.URL)
		// No kind is resend-safe, but the provider dedupes, so it is retried.
		f := newFixture(t, dialect, p, Options{})
		f.queue(t, "a")
		f.process(t)
		row := f.row(t, "a@example.com")
		if row.Status != statusFailed || row.LastErrorCode != "timeout" {
			t.Fatalf("after accepted-then-timeout: %+v", row)
		}
		f.clock.add(time.Minute)
		f.process(t)
		row = f.row(t, "a@example.com")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if row.Status != statusSent || row.ProviderMessageID != "re_1" {
			t.Fatalf("retry: %+v", row)
		}
		if len(fake.requests) != 2 || fake.requests[0] != fake.requests[1] || fake.requests[0] != row.ID || len(fake.delivered) != 1 {
			t.Fatalf("requests %v delivered %v", fake.requests, fake.delivered)
		}
	})
}

// A provider deduplicates only for a while (Resend: 24 hours). Once that
// window has passed since the earliest attempt it may have accepted, sending
// again could deliver twice, so the row goes to unknown unless the kind is
// resend-safe. Both the retry and the lease-expired paths check it.
func TestIdempotencyWindowLimitsResends(t *testing.T) {
	const window = 24 * time.Hour
	forEachDialect(t, func(t *testing.T, dialect string) {
		t.Run("retry within window", func(t *testing.T) {
			p := &fakeProvider{window: window, script: []error{ambiguous("timeout")}}
			f := newFixture(t, dialect, p, Options{})
			f.queue(t, "a")
			f.process(t)
			if row := f.row(t, "a@example.com"); row.Status != statusFailed || row.MaybeAcceptedAt == nil {
				t.Fatalf("after ambiguous: %+v", row)
			}
			f.clock.add(time.Hour)
			f.process(t)
			if row := f.row(t, "a@example.com"); row.Status != statusSent || p.count() != 2 {
				t.Fatalf("retry inside the window: %+v sends=%d", row, p.count())
			}
		})
		t.Run("retry after window", func(t *testing.T) {
			p := &fakeProvider{window: window, script: []error{ambiguous("timeout")}}
			f := newFixture(t, dialect, p, Options{})
			f.queue(t, "a")
			f.process(t)
			f.clock.add(window) // the worker was down for a day
			f.process(t)
			row := f.row(t, "a@example.com")
			if row.Status != statusUnknown || row.LastErrorCode != "resend_unsafe" || p.count() != 1 {
				t.Fatalf("retry outside the window: %+v sends=%d", row, p.count())
			}
		})
		t.Run("window counts from the earliest maybe-accepted attempt", func(t *testing.T) {
			p := &fakeProvider{window: window, script: []error{ambiguous("timeout"), retryable("http_429")}}
			f := newFixture(t, dialect, p, Options{})
			f.queue(t, "a")
			f.process(t) // ambiguous at t0
			first := f.row(t, "a@example.com").MaybeAcceptedAt
			f.clock.add(23 * time.Hour)
			f.process(t) // certainly not accepted; keeps t0
			if row := f.row(t, "a@example.com"); row.Status != statusFailed || !row.MaybeAcceptedAt.Equal(*first) {
				t.Fatalf("after retryable: %+v (first %v)", row, first)
			}
			f.clock.add(2 * time.Hour)
			f.process(t)
			if row := f.row(t, "a@example.com"); row.Status != statusUnknown || p.count() != 2 {
				t.Fatalf("25h after the first attempt: %+v sends=%d", row, p.count())
			}
		})
		t.Run("resend-safe kind ignores the window", func(t *testing.T) {
			p := &fakeProvider{window: window, script: []error{ambiguous("timeout")}}
			f := newFixture(t, dialect, p, Options{ResendSafe: func(string) bool { return true }})
			f.queue(t, "a")
			f.process(t)
			f.clock.add(2 * window)
			f.process(t)
			if row := f.row(t, "a@example.com"); row.Status != statusSent || p.count() != 2 {
				t.Fatalf("resend-safe: %+v sends=%d", row, p.count())
			}
		})
		t.Run("window ends during the checks before sending", func(t *testing.T) {
			p := &fakeProvider{window: window, script: []error{ambiguous("timeout")}}
			f := newFixture(t, dialect, p, Options{})
			// The suppression lookup takes 3s of the 2s left in the window.
			f.outbox.suppressions = slowSuppressions{clock: f.clock, delay: 3 * time.Second}
			f.queue(t, "a")
			f.process(t)
			first := *f.row(t, "a@example.com").MaybeAcceptedAt
			f.clock.set(first.Add(window - f.outbox.opts.SendTimeout - 2*time.Second))
			f.process(t)
			row := f.row(t, "a@example.com")
			if row.Status != statusUnknown || row.LastErrorCode != "resend_unsafe" || p.count() != 1 {
				t.Fatalf("window ran out before the request: %+v sends=%d", row, p.count())
			}
		})
		t.Run("expired lease within window", func(t *testing.T) {
			p := &fakeProvider{window: window}
			f := newFixture(t, dialect, p, Options{})
			f.queue(t, "a")
			if row, _, err := f.outbox.claim(context.Background()); err != nil || row == nil {
				t.Fatalf("claim: %v %v", row, err)
			}
			f.clock.add(2 * time.Minute)
			f.process(t)
			row := f.row(t, "a@example.com")
			if row.Status != statusSent || row.MaybeAcceptedAt == nil || p.count() != 1 {
				t.Fatalf("reclaimed inside the window: %+v sends=%d", row, p.count())
			}
		})
		t.Run("expired lease after window", func(t *testing.T) {
			p := &fakeProvider{window: window}
			f := newFixture(t, dialect, p, Options{})
			f.queue(t, "a")
			if row, _, err := f.outbox.claim(context.Background()); err != nil || row == nil {
				t.Fatalf("claim: %v %v", row, err)
			}
			f.clock.add(window)
			f.process(t)
			row := f.row(t, "a@example.com")
			if row.Status != statusUnknown || row.LastErrorCode != "lease_expired" || p.count() != 0 {
				t.Fatalf("reclaimed outside the window: %+v sends=%d", row, p.count())
			}
		})
	})
}

// slowSuppressions suppresses nothing but moves the clock forward, like a
// slow lookup.
type slowSuppressions struct {
	clock *clock
	delay time.Duration
}

func (s slowSuppressions) Suppressed(context.Context, *gorm.DB, string) (bool, error) {
	s.clock.add(s.delay)
	return false, nil
}

func TestBacklogLimits(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		f := newFixture(t, dialect, &fakeProvider{}, Options{Backlog: 4, PerRecipient: 2})
		ctx := context.Background()
		keyed := func(to, key string) Message {
			m := message(to, "t")
			m.Outbound.IdempotencyKey = key
			return m
		}
		for _, key := range []string{"k1", "k2"} {
			if err := f.outbox.Deliver(ctx, nil, keyed("same@example.com", key)); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.outbox.Deliver(ctx, nil, keyed("SAME@example.com", "k3")); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("per-recipient limit: %v", err)
		}
		// Repeating a queued key succeeds even when full.
		if err := f.outbox.Deliver(ctx, nil, keyed("same@example.com", "k1")); err != nil {
			t.Fatalf("repeat of k1: %v", err)
		}
		for _, to := range []string{"a@example.com", "b@example.com"} {
			if err := f.outbox.Deliver(ctx, nil, message(to, "t")); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.outbox.Deliver(ctx, nil, message("c@example.com", "t")); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("backlog limit: %v", err)
		}
		// A row waiting for a retry no longer counts.
		if err := f.db.Model(&outboxRow{}).Where("recipient = ?", "a@example.com").Update("status", statusFailed).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.outbox.Deliver(ctx, nil, message("c@example.com", "t")); err != nil {
			t.Fatalf("after one row left the backlog: %v", err)
		}
	})
}

// Concurrent enqueues must not all pass the same limit.
func TestConcurrentEnqueuesRespectBacklog(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		const limit = 4
		f := newFixture(t, dialect, &fakeProvider{}, Options{Backlog: limit})
		ctx := context.Background()
		for i := 0; i < limit-1; i++ {
			if err := f.outbox.Deliver(ctx, nil, message(fmt.Sprintf("q%d@example.com", i), "t")); err != nil {
				t.Fatal(err)
			}
		}
		const senders = 12
		var wg sync.WaitGroup
		var mu sync.Mutex
		accepted, full := 0, 0
		start := make(chan struct{})
		for i := 0; i < senders; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				err := f.outbox.Deliver(ctx, nil, message(fmt.Sprintf("c%d@example.com", i), "t"))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					accepted++
				case errors.Is(err, ErrQueueFull):
					full++
				default:
					t.Errorf("send %d: %v", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if n := f.count(t, ""); accepted != 1 || full != senders-1 || n != limit {
			t.Fatalf("accepted %d, full %d, rows %d (limit %d)", accepted, full, n, limit)
		}
	})
}

// Concurrent claims never hand the same row to two workers.
func TestConcurrentClaimsTakeEachRowOnce(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		f := newFixture(t, dialect, &fakeProvider{}, Options{})
		ctx := context.Background()
		const rows = 15
		for i := 0; i < rows; i++ {
			if err := f.outbox.Deliver(ctx, nil, message(fmt.Sprintf("r%d@example.com", i), "t")); err != nil {
				t.Fatal(err)
			}
		}
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					row, _, err := f.outbox.claim(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					if row == nil {
						return
					}
					mu.Lock()
					seen[row.ID]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(seen) != rows {
			t.Fatalf("claimed %d distinct rows, want %d", len(seen), rows)
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("row %s claimed %d times", id, n)
			}
		}
	})
}

func TestWebhookBeforeTheOutcomeIsKnown(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{script: []error{ambiguous("timeout_after_write"), ambiguous("timeout_after_write")}}
		f := newFixture(t, dialect, p, Options{})
		ctx := context.Background()
		delivered := func(id string) {
			t.Helper()
			if err := f.outbox.Resolve(ctx, email.Event{Type: email.EventDelivered, Tags: map[string]string{"outbox_id": id}}); err != nil {
				t.Fatal(err)
			}
		}

		// Webhook while the worker still waits for the provider; the
		// worker's ambiguous result then loses its claim.
		f.queue(t, "inflight")
		row, _, err := f.outbox.claim(ctx)
		if err != nil || row == nil {
			t.Fatalf("claim: %+v %v", row, err)
		}
		delivered(row.ID)
		if got := f.row(t, "inflight@example.com"); got.Status != statusSent {
			t.Fatalf("webhook during send: %+v", got)
		}
		f.outbox.send(ctx, row, false)
		if got := f.row(t, "inflight@example.com"); got.Status != statusSent {
			t.Fatalf("late worker result overwrote the webhook: %+v", got)
		}
		if sent, failed := f.receipt(t, "inflight"); !sent || failed {
			t.Fatal("receipt after webhook during send")
		}

		// Worker crashed holding the row; the webhook arrives before its
		// lease expires. The row must not become unknown and then dead.
		f.queue(t, "crashed")
		row, _, err = f.outbox.claim(ctx)
		if err != nil || row == nil {
			t.Fatalf("claim: %+v %v", row, err)
		}
		delivered(row.ID)
		f.clock.add(2 * time.Minute)
		if f.process(t) {
			t.Fatal("settled row was claimed again")
		}
		if got := f.row(t, "crashed@example.com"); got.Status != statusSent {
			t.Fatalf("webhook before lease expiry: %+v", got)
		}

		// Timed out on a resend-safe kind and waiting to retry when the
		// webhook proves delivery: no second send.
		f.outbox.opts.ResendSafe = func(string) bool { return true }
		f.queue(t, "retry")
		f.process(t)
		got := f.row(t, "retry@example.com")
		if got.Status != statusFailed {
			t.Fatalf("ambiguous, resend-safe: %+v", got)
		}
		delivered(got.ID)
		attempts := p.count()
		f.clock.add(time.Hour)
		f.process(t)
		if got := f.row(t, "retry@example.com"); got.Status != statusSent || p.count() != attempts {
			t.Fatalf("delivered email retried: %+v (%d sends)", got, p.count()-attempts)
		}
	})
}

func TestDropsExpiredMailAndErasesBodies(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{script: []error{retryable("x")}}
		f := newFixture(t, dialect, p, Options{})
		expires := f.clock.now().Add(10 * time.Minute)
		for _, id := range []string{"late", "unsent"} {
			f.queue(t, id, func(m *Message) { m.ExpiresAt = expires })
		}
		f.process(t) // "late" fails once and waits for a retry
		f.clock.set(expires.Add(time.Second))
		if err := f.outbox.sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"late", "unsent"} {
			row := f.row(t, id+"@example.com")
			if row.Status != statusDead || row.LastErrorCode != "expired" || row.Payload != nil {
				t.Fatalf("%s after expiry: %+v", id, row)
			}
			if _, failed := f.receipt(t, id); !failed {
				t.Fatalf("%s: expiry receipt", id)
			}
		}
		if p.count() != 1 {
			t.Fatalf("expired mail was sent: %d attempts", p.count())
		}
		// Finished rows are deleted after the retention period.
		f.clock.add(31 * 24 * time.Hour)
		if err := f.outbox.sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := f.count(t, ""); n != 0 {
			t.Fatalf("%d rows kept past retention", n)
		}
	})
}

type suppressList map[string]bool

func (s suppressList) Suppressed(_ context.Context, _ *gorm.DB, recipient string) (bool, error) {
	return s[recipient], nil
}

func TestSuppressionsAndProviderRouting(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		first := &fakeProvider{name: "first"}
		f := newFixture(t, dialect, first, Options{Suppressions: suppressList{"blocked@example.com": true}})
		second := &fakeProvider{name: "second"}
		f.outbox.Providers().Add(second)

		f.queue(t, "blocked")
		f.process(t)
		if row := f.row(t, "blocked@example.com"); row.Status != statusDead || row.LastErrorCode != "suppressed" || first.count() != 0 {
			t.Fatalf("suppressed address: %+v", row)
		}
		if _, failed := f.receipt(t, "blocked"); !failed {
			t.Fatal("suppressed receipt")
		}

		f.queue(t, "routed", func(m *Message) { m.Provider = "second" })
		f.process(t)
		if row := f.row(t, "routed@example.com"); row.Status != statusSent || second.count() != 1 || first.count() != 0 {
			t.Fatalf("named provider: %+v", row)
		}

		// A provider that is no longer configured is retried later.
		f.queue(t, "orphan", func(m *Message) { m.Provider = "removed" })
		f.process(t)
		if row := f.row(t, "orphan@example.com"); row.Status != statusFailed || row.LastErrorCode != "provider_unavailable" || row.Payload == nil {
			t.Fatalf("missing provider: %+v", row)
		}
	})
}

func TestWorkersDeliverAndStop(t *testing.T) {
	p := &fakeProvider{}
	db := openTestDB(t, "sqlite")
	o, err := New(db, p, testKey(), quietLogger(), Options{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !o.CanDeliver() {
		t.Fatal("fake provider cannot deliver")
	}
	o.Start()
	ctx := context.Background()
	for _, to := range []string{"a@example.com", "b@example.com"} {
		if err := o.Deliver(ctx, nil, message(to, "t")); err != nil {
			t.Fatal(err)
		}
	}
	for deadline := time.Now().Add(3 * time.Second); p.count() < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if p.count() != 2 {
		t.Fatalf("workers sent %d emails", p.count())
	}
	if err := o.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := o.Deliver(ctx, nil, message("c@example.com", "t")); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("queued after close: %v", err)
	}
	if _, err := New(db, p, []byte("short"), nil, Options{}); err == nil {
		t.Fatal("short key accepted")
	}
	disabled, err := New(db, email.Disabled{}, testKey(), nil, Options{})
	if err != nil || disabled.CanDeliver() {
		t.Fatalf("disabled provider: %v", err)
	}
	var payload string
	if err := db.Model(&outboxRow{}).Select("COALESCE(payload, '')").Where("recipient = ?", "a@example.com").Scan(&payload).Error; err != nil || payload != "" {
		t.Fatalf("payload kept after send: %q %v", payload, err)
	}
}

// A body sealed for one row cannot be replayed on another, and a wrong key
// marks the row dead instead of retrying forever.
func TestUndecryptableBodyIsDead(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect string) {
		p := &fakeProvider{}
		f := newFixture(t, dialect, p, Options{})
		f.queue(t, "a")
		f.queue(t, "b")
		a, b := f.row(t, "a@example.com"), f.row(t, "b@example.com")
		if err := f.db.Model(&outboxRow{}).Where("id = ?", b.ID).Update("payload", *a.Payload).Error; err != nil {
			t.Fatal(err)
		}
		for f.process(t) {
		}
		if row := f.row(t, "b@example.com"); row.Status != statusDead || row.LastErrorCode != "undecryptable" {
			t.Fatalf("moved body: %+v", row)
		}
		if p.count() != 1 || p.attempts[0].To.Address != "a@example.com" {
			t.Fatalf("attempts: %+v", p.attempts)
		}
	})
}
