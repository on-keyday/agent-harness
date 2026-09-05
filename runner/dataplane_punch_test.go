package runner

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

type fakeProbeSender struct {
	n    atomic.Int32
	last atomic.Pointer[objproto.ConnectionID]
}

func (f *fakeProbeSender) SendProbe(cid objproto.ConnectionID, _ [6]byte, _ netip.AddrPort) error {
	f.n.Add(1)
	f.last.Store(&cid)
	return nil
}

func punchTestTarget() protocol.RunnerID {
	return protocol.ConnIDToRunnerID(objproto.NewConnectionID("udp",
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 45999), 0x7777))
}

// The only value this version's server writes is an absent target, so the
// no-op path is the one that actually runs in production.
func TestPunchTowardSendsNothingWhenTargetAbsent(t *testing.T) {
	f := &fakeProbeSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if n := punchToward(ctx, f, protocol.RunnerID{}, 5*time.Millisecond); n != 0 {
		t.Fatalf("absent target should send nothing, sent %d", n)
	}
	if got := f.n.Load(); got != 0 {
		t.Fatalf("SendProbe called %d times for an absent target", got)
	}
}

func TestPunchTowardSendsUntilContextEnds(t *testing.T) {
	f := &fakeProbeSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	n := punchToward(ctx, f, punchTestTarget(), 10*time.Millisecond)
	if n < 3 {
		t.Fatalf("expected several probes in 120ms at a 10ms interval, got %d", n)
	}
	if int32(n) != f.n.Load() {
		t.Fatalf("return %d disagrees with calls %d", n, f.n.Load())
	}
}

// The probe has to reach the exact socket the peer will dial from, so the
// address on the wire must survive the RunnerID round trip unchanged.
func TestPunchTowardAddressesTheGivenSocket(t *testing.T) {
	f := &fakeProbeSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	punchToward(ctx, f, punchTestTarget(), 5*time.Millisecond)
	got := f.last.Load()
	if got == nil {
		t.Fatal("no probe recorded")
	}
	if got.Addr.Port() != 45999 || got.Addr.Addr().String() != "127.0.0.1" {
		t.Fatalf("probe went to the wrong socket: %s", got.String())
	}
	if got.Transport != "udp" {
		t.Fatalf("probe used transport %q", got.Transport)
	}
}
