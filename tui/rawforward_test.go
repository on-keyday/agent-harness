package tui

import (
	"bytes"
	"testing"
)

func TestRawConnectModal_OpenCloseAndSpec(t *testing.T) {
	m := NewRawConnectModal()
	if m.IsOpen() {
		t.Fatal("fresh modal must be closed")
	}
	m.Open("4a1f0000000000000000000000000000")
	if !m.IsOpen() || m.TaskID() == "" {
		t.Fatal("Open must record the task and open")
	}
	m.SetSpec("127.0.0.1:6379")
	host, port, err := m.Target()
	if err != nil || host != "127.0.0.1" || port != 6379 {
		t.Fatalf("Target() = %s:%d err=%v", host, port, err)
	}
	m.SetSpec("garbage")
	if _, _, err := m.Target(); err == nil {
		t.Fatal("Target() must reject a spec that is not host:port")
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("Close must close")
	}
}

// TestRawConnectModal_RingCapKeepsNewest checks the bound AND which end
// survives: a trim that kept the oldest bytes would satisfy a length-only
// assertion while showing the operator stale output.
//
// The brief's literal version of this test filled `head` with a single
// repeated byte ('H') and asserted !HasPrefix(out, head[:16]). That assertion
// is unfalsifiable: filler built from one repeated byte makes every 16-byte
// window of `head` identical, so the front of a CORRECTLY trimmed buffer
// (which is still deep inside the same all-'H' run, just starting 11 bytes
// later) still matches head[:16] — the check fires against a correct
// implementation, not just a buggy one (verified against this exact
// implementation: it does). Fixed here by giving the discarded front an
// 11-byte marker distinct from its filler and from the tail, and asserting
// its literal absence (bytes.Contains) instead of a prefix match against
// indistinguishable filler.
func TestRawConnectModal_RingCapKeepsNewest(t *testing.T) {
	m := NewRawConnectModal()
	m.Open("4a1f0000000000000000000000000000")
	headMarker := []byte("HEAD-MARKER") // same length as tail below, by design
	head := append(append([]byte(nil), headMarker...), bytes.Repeat([]byte("H"), rawTUIRingBytes-len(headMarker))...)
	m.AppendOutput(head)
	tail := []byte("TAIL-MARKER")
	m.AppendOutput(tail)
	out := m.Output()
	if len(out) > rawTUIRingBytes {
		t.Fatalf("output ring = %d bytes, want <= %d", len(out), rawTUIRingBytes)
	}
	if !bytes.HasSuffix(out, tail) {
		t.Fatalf("newest bytes must survive the trim; output ends with %q", out[max(0, len(out)-16):])
	}
	if bytes.Contains(out, headMarker) {
		t.Fatal("oldest bytes must be trimmed from the front")
	}
}

// TestRawForwardMsgs_StaleGenerationIgnored is the regression for the
// esc-then-reopen workflow: a connection abandoned for task A must not splice
// its trailing bytes, or its close notice, into the session the operator opened
// for task B.
func TestRawForwardMsgs_StaleGenerationIgnored(t *testing.T) {
	a := New(Config{}) // tui/portforward_test.go's app-construction convention
	a.rawGen = 2
	a.rawModal.Open("bbbb0000000000000000000000000000")
	a.rawModal.MarkLive("connected (fwd 9)")

	a.Update(RawForwardDataMsg{Gen: 1, Data: []byte("stale")})
	if len(a.rawModal.Output()) != 0 {
		t.Fatalf("stale data applied: %q", a.rawModal.Output())
	}
	a.Update(RawForwardClosedMsg{Gen: 1, Reason: "stale close"})
	if !a.rawModal.IsLive() {
		t.Fatal("a stale close must not mark the live session closed")
	}

	a.Update(RawForwardDataMsg{Gen: 2, Data: []byte("fresh")})
	if got := string(a.rawModal.Output()); got != "fresh" {
		t.Fatalf("current-generation data not applied: %q", got)
	}
	a.Update(RawForwardClosedMsg{Gen: 2, Reason: "done"})
	if a.rawModal.IsLive() {
		t.Fatal("current-generation close must mark the session closed")
	}
}
