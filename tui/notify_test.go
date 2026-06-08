package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestRenderNotifyEvent_WorkerShowsTaskIDAndCorrectTime guards two display
// fixes: the worker block's short task id is shown, and ev.Ts is rendered as
// unix seconds (not nanoseconds — the earlier time.Unix(0, ev.Ts) bug rendered
// every event near the epoch).
func TestRenderNotifyEvent_WorkerShowsTaskIDAndCorrectTime(t *testing.T) {
	const taskID = "0f0d4dd6b7d3b64354cf4ff249b87403"
	var ts int64 = 1717800000 // unix seconds
	ev := protocol.NotifyEvent{
		Ts:       uint64(ts),
		Level:    protocol.NotifyLevel_Info,
		Origin:   protocol.NotifyOrigin_Worker,
		TitleLen: 1,
		Title:    []byte("t"),
		TextLen:  2,
		Text:     []byte("hi"),
	}
	ev.SetWorker(protocol.WorkerInfo{
		TaskIdLen:   uint16(len(taskID)),
		TaskId:      []byte(taskID),
		HostnameLen: uint16(len("gmkhost")),
		Hostname:    []byte("gmkhost"),
	})
	got := renderNotifyEvent(ev)

	if !strings.Contains(got, taskID[:8]) {
		t.Fatalf("render missing short task id %q: %q", taskID[:8], got)
	}
	if !strings.Contains(got, "gmkhost") {
		t.Fatalf("render missing hostname: %q", got)
	}
	wantTime := time.Unix(ts, 0).Local().Format("15:04:05")
	if !strings.Contains(got, wantTime) {
		t.Fatalf("ts not rendered as unix seconds: got %q, want time %q", got, wantTime)
	}
}

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
