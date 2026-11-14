package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A `session new --oneshot` task is kind interactive (A-D29). The table's
// About cell shows its prompt, marked, where an ordinary session shows
// <interactive>; the `d` popup's kind line carries the same mark as `ls`.
func TestOneshotSessionRowAndDetail(t *testing.T) {
	one := progressTask(protocol.TaskKind_Interactive, "", 0, false, "")
	one.SetOneshot(true)
	plain := progressTask(protocol.TaskKind_Interactive, "", 0, false, "")
	if got := aboutCell(one); got != "(oneshot) the original ask" {
		t.Errorf("one-shot About = %q", got)
	}
	if got := aboutCell(plain); got != "<interactive>" {
		t.Errorf("plain About = %q", got)
	}
	if d := formatTaskDetail(one, nil); !strings.Contains(d, "kind:          interactive+oneshot\n") {
		t.Errorf("one-shot detail kind line:\n%s", d)
	}
	if d := formatTaskDetail(plain, nil); !strings.Contains(d, "kind:          interactive\n") {
		t.Errorf("plain detail kind line:\n%s", d)
	}
}

// The flag is per run (a resume restates it or drops it), and the TUI updates a
// known row from events alone, so every event sets it — and clears it.
func TestApplyEventOneshot(t *testing.T) {
	a := New(Config{})
	var ti protocol.TaskInfo
	ev := protocol.TaskStatusEvent{TaskStatus: protocol.TaskStatus_Running}
	ev.SetOneshot(true)
	a.applyEventProgress(&ti, "x", ev)
	if !ti.Oneshot() {
		t.Fatal("event oneshot=true did not reach the row")
	}
	ev.SetOneshot(false)
	a.applyEventProgress(&ti, "x", ev)
	if ti.Oneshot() {
		t.Fatal("event oneshot=false did not clear the row")
	}
}
