package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestDrainNotifyEventsCoalesced verifies that drainNotifyEvents correctly
// handles a buffer containing multiple coalesced events (as produced by the
// server-side ring replay path) plus a trailing partial event.
func TestDrainNotifyEventsCoalesced(t *testing.T) {
	// Use NotifyOrigin_External — NotifyOrigin_Worker (zero value) triggers
	// a union-type assertion in Append that panics on zero-value structs.
	e1 := (&protocol.NotifyEvent{
		Ts:      10,
		Level:   protocol.NotifyLevel_Info,
		Origin:  protocol.NotifyOrigin_External,
		TitleLen: 5,
		Title:   []byte("hello"),
		TextLen: 5,
		Text:    []byte("world"),
	}).MustAppend(nil)
	e2 := (&protocol.NotifyEvent{
		Ts:      20,
		Level:   protocol.NotifyLevel_Warn,
		Origin:  protocol.NotifyOrigin_External,
		TitleLen: 3,
		Title:   []byte("foo"),
		TextLen: 3,
		Text:    []byte("bar"),
	}).MustAppend(nil)

	// Coalesce e1 + e2 + partial third event into one buffer.
	buf := append(append([]byte{}, e1...), e2...)
	buf = append(buf, e2[:1]...) // one byte of a partial third — must not be decoded

	var emitted []protocol.NotifyEvent
	rest := drainNotifyEvents(buf, func(ev protocol.NotifyEvent) {
		emitted = append(emitted, ev)
	})

	// Exactly 2 events must have been emitted.
	if len(emitted) != 2 {
		t.Fatalf("emitted %d events, want 2", len(emitted))
	}

	// Events must be in order (by Ts).
	if emitted[0].Ts != 10 {
		t.Errorf("emitted[0].Ts = %d, want 10", emitted[0].Ts)
	}
	if emitted[1].Ts != 20 {
		t.Errorf("emitted[1].Ts = %d, want 20", emitted[1].Ts)
	}

	// The returned remainder must equal the 1 partial byte.
	if len(rest) != 1 {
		t.Fatalf("remainder = %d bytes, want 1 (partial event)", len(rest))
	}
}
