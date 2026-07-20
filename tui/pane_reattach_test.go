package tui

import (
	"context"
	"testing"
	"time"
)

// nextBackoff doubles up to the cap and then stays there — so a persistent
// drop-storm settles at paneReattachMax instead of hammering the shared link.
func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(paneReattachBase); got != paneReattachBase*2 {
		t.Fatalf("expected doubling to %v, got %v", paneReattachBase*2, got)
	}
	// Iterating must converge to (and never exceed) the cap.
	d := paneReattachBase
	for i := 0; i < 20; i++ {
		d = nextBackoff(d)
		if d > paneReattachMax {
			t.Fatalf("backoff %v exceeded cap %v", d, paneReattachMax)
		}
	}
	if d != paneReattachMax {
		t.Fatalf("backoff must saturate at %v, got %v", paneReattachMax, d)
	}
}

// sleepCtx returns true when the timer fires and false when the context ends
// first — the pump uses the false return to stop reattaching on Stop()/Close()
// rather than sleeping out a full backoff.
func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("sleepCtx must return true when the timer fires")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx must return false when ctx is already done")
	}
	if time.Since(start) > time.Second {
		t.Fatal("sleepCtx must return promptly on a done ctx, not wait out the delay")
	}
}
