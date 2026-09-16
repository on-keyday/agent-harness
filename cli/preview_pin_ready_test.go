package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A preview pin is entered in the registry BEFORE its RegisterPortForward round
// trip and completed after it, and the page starts running — and fetching —
// inside that window. These cover the gap: a fetch that arrives while the
// registration is in flight must WAIT for it, not be told there is no pin.

func TestPinReadyWaitReturnsAfterSettle(t *testing.T) {
	r := newPinReady()
	start := make(chan struct{})
	var got error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(start)
		got = r.wait(context.Background())
	}()
	<-start
	// Settling from another goroutine is the real shape: OpenPreviewPin settles
	// on the RPC's goroutine while a fetch is already parked here.
	time.AfterFunc(10*time.Millisecond, func() { r.settle(nil) })
	wg.Wait()
	if got != nil {
		t.Fatalf("wait after a successful settle: got %v, want nil", got)
	}
}

func TestPinReadyWaitIsImmediateOnceSettled(t *testing.T) {
	r := newPinReady()
	r.settle(nil)
	ctx, cancel := context.WithCancel(context.Background())
	// An already-cancelled context must NOT turn a settled pin into a failure:
	// the registration is done, so there is nothing left to wait for.
	cancel()
	if err := r.wait(ctx); err != nil {
		t.Fatalf("wait on a settled pin with a cancelled ctx: got %v, want nil", err)
	}
}

func TestPinReadyWaitReportsTheRegistrationError(t *testing.T) {
	want := errors.New("register: refused")
	r := newPinReady()
	r.settle(want)
	// The caller must see WHY there is no pin. Collapsing this into a generic
	// "no live pin" is what made the original race unreadable: the page was
	// told the pin did not exist when it was being established, and told the
	// same thing when the server refused it.
	if err := r.wait(context.Background()); !errors.Is(err, want) {
		t.Fatalf("wait after a failed settle: got %v, want %v", err, want)
	}
}

func TestPinReadyWaitHonoursContext(t *testing.T) {
	r := newPinReady()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// A registration that never lands must not park a fetch forever.
	if err := r.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait on a pin that never settles: got %v, want DeadlineExceeded", err)
	}
}

func TestPinReadySettleIsIdempotentAndKeepsTheFirstOutcome(t *testing.T) {
	first := errors.New("first")
	r := newPinReady()
	r.settle(first)
	r.settle(nil) // a late second settle must not rewrite the answer
	r.settle(errors.New("third"))
	if err := r.wait(context.Background()); !errors.Is(err, first) {
		t.Fatalf("after three settles: got %v, want the first (%v)", err, first)
	}
}

func TestPinReadyConcurrentWaitersAllSeeTheSameOutcome(t *testing.T) {
	want := errors.New("register: refused")
	r := newPinReady()
	// PREVIEW_FETCH_MAX_INFLIGHT is 4 on the page side, so several fetches can
	// be parked on one registration at once.
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = r.wait(context.Background())
		}()
	}
	time.AfterFunc(10*time.Millisecond, func() { r.settle(want) })
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, want) {
			t.Fatalf("waiter %d: got %v, want %v", i, err, want)
		}
	}
}
