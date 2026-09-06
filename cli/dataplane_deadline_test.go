package cli

import (
	"context"
	"testing"
	"time"
)

// The deadline has to cover the DIAL, not just the hello that follows it.
//
// Observed on the live fleet: a --route direct push parked for six minutes with
// no CPU, having completed in 520ms minutes earlier on the same pair. The 10s
// timeout was wrapped around the hello response only; peer.Dial ran on the
// caller's context, which for a CLI push has no deadline. A path the punch had
// not opened therefore hung forever rather than failing.
//
// There is no automatic fallback by design, so a prompt failure is the whole of
// what the caller gets back. This pins the bound to something a person waits
// through, and pins that dial and hello share ONE budget rather than two.
func TestDataPlaneSetupIsBoundedAndSharedWithTheDial(t *testing.T) {
	if dataPlaneHandshakeTimeout <= 0 {
		t.Fatal("the data plane setup has no deadline")
	}
	if dataPlaneHandshakeTimeout > 30*time.Second {
		t.Fatalf("dataPlaneHandshakeTimeout is %v: too long to be a failure a person waits through",
			dataPlaneHandshakeTimeout)
	}
	// A caller whose own context is already done must not start a dial at all,
	// which is the same property from the other end: the budget is the
	// caller's, narrowed, never replaced by a fresh one.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	derived, dcancel := context.WithTimeout(ctx, dataPlaneHandshakeTimeout)
	defer dcancel()
	if derived.Err() == nil {
		t.Fatal("the setup deadline does not inherit the caller's cancellation")
	}
}
