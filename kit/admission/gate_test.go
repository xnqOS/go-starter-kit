package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/apperror"
)

func waitForQueue(t *testing.T, g *Gate) {
	t.Helper()
	deadline := time.After(time.Second)
	for g.Waiting() == 0 {
		select {
		case <-deadline:
			t.Fatal("request did not enter the queue")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestGateWaitsForAFreeSlot(t *testing.T) {
	g := New(1, 1, time.Second)
	release, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Do(context.Background(), func() error { return nil }) }()
	waitForQueue(t, g)
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(g.slots) != 0 || len(g.waiting) != 0 {
		t.Fatal("gate leaked a permit")
	}
}

func TestGateCancelsAndBoundsQueue(t *testing.T) {
	g := New(1, 1, time.Second)
	release, _ := g.Acquire(context.Background())
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.Acquire(ctx); done <- err }()
	waitForQueue(t, g)
	if _, err := g.Acquire(context.Background()); !errors.Is(err, apperror.ErrUnavailable) {
		t.Fatalf("queue overflow: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queue ignored cancellation: %v", err)
	}
	if len(g.waiting) != 0 {
		t.Fatal("cancelled waiter kept its queue place")
	}
	if _, err := g.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("ended context admitted")
	}
}

func TestGateTimesOutAndZeroQueueRefuses(t *testing.T) {
	g := New(1, 1, 20*time.Millisecond)
	release, _ := g.Acquire(context.Background())
	defer release()
	if _, err := g.Acquire(context.Background()); !errors.Is(err, apperror.ErrUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
	z := New(1, 0, time.Second)
	free, _ := z.Acquire(context.Background())
	defer free()
	if _, err := z.Acquire(context.Background()); !errors.Is(err, apperror.ErrUnavailable) {
		t.Fatalf("zero queue: %v", err)
	}
}

func TestGateReportsWaitingAndInUse(t *testing.T) {
	g := New(1, 1, time.Second)
	release, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		free, err := g.Acquire(context.Background())
		if err == nil {
			free()
		}
		close(done)
	}()
	waitForQueue(t, g)
	if g.InUse() != 1 || g.Waiting() != 1 {
		t.Fatalf("InUse=%d Waiting=%d, want 1 and 1", g.InUse(), g.Waiting())
	}
	release()
	<-done
	if g.InUse() != 0 || g.Waiting() != 0 {
		t.Fatalf("InUse=%d Waiting=%d after release, want 0 and 0", g.InUse(), g.Waiting())
	}
}
