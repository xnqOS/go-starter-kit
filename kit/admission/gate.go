// Package admission bounds how many expensive operations (password hashing,
// image processing, calls to a slow dependency) run at once.
package admission

import (
	"context"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

// Gate admits at most concurrency callers at a time. Up to queue more wait
// for at most wait; anyone beyond that is refused immediately. Refusals
// return apperror.ErrUnavailable (HTTP 503 with Retry-After), so overload
// turns into a quick retryable answer instead of a pile-up of goroutines.
type Gate struct {
	slots, waiting chan struct{}
	wait           time.Duration
}

// New panics on non-positive concurrency or wait, which are programming errors.
// queue may be 0 to refuse whenever every slot is busy.
func New(concurrency, queue int, wait time.Duration) *Gate {
	if concurrency <= 0 || queue < 0 || wait <= 0 {
		panic("admission: concurrency and wait must be positive, queue non-negative")
	}
	return &Gate{slots: make(chan struct{}, concurrency), waiting: make(chan struct{}, queue), wait: wait}
}

// Acquire waits for a slot. On success the caller must call release exactly
// once. It returns ctx.Err() when ctx ends first.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release = func() { <-g.slots }
	select {
	case g.slots <- struct{}{}:
		return release, nil
	default:
	}
	select {
	case g.waiting <- struct{}{}:
		defer func() { <-g.waiting }()
	default:
		return nil, apperror.ErrUnavailable
	}
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, apperror.ErrUnavailable
	}
}

// Do runs fn while holding a slot.
func (g *Gate) Do(ctx context.Context, fn func() error) error {
	release, err := g.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
