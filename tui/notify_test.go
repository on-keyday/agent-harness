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

	if !strings.Contains(got, taskID) {
		t.Fatalf("render missing full task id %q: %q", taskID, got)
	}
	if !strings.Contains(got, "gmkhost") {
		t.Fatalf("render missing hostname: %q", got)
	}
	wantTime := time.Unix(ts, 0).Local().Format("15:04:05")
	if !strings.Contains(got, wantTime) {
		t.Fatalf("ts not rendered as unix seconds: got %q, want time %q", got, wantTime)
	}
}

// TestRenderNotifyEvent_NoTitleNoDanglingSeparator: an untitled event shows the
// text cleanly, with no dangling "— " separator (the old format rendered
// "[info]  — text" which read as malformed).
func TestRenderNotifyEvent_NoTitleNoDanglingSeparator(t *testing.T) {
	ev := protocol.NotifyEvent{
		Ts:      1717800000,
		Level:   protocol.NotifyLevel_Info,
		Origin:  protocol.NotifyOrigin_External,
		TextLen: uint16(len("body only")),
		Text:    []byte("body only"),
	}
	got := renderNotifyEvent(ev)
	if !strings.Contains(got, "body only") {
		t.Fatalf("render missing text: %q", got)
	}
	if strings.Contains(got, "— body only") {
		t.Fatalf("untitled event has a dangling separator: %q", got)
	}
}

// Notification text is task-written (and the topic can be fed by any
// subscriber); the pane writes it straight to the terminal, so it strips
// control characters itself.
func TestRenderNotifyEvent_StripsControls(t *testing.T) {
	ev := protocol.NotifyEvent{Ts: 1, Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_External}
	ev.SetTitle([]byte("t\x1b[2J"))
	ev.SetText([]byte("x\u009by"))
	if got := renderNotifyEvent(ev); strings.ContainsAny(got, "\x1b\u009b") {
		t.Fatalf("rendered %q, want no ESC/C1", got)
	}
}
