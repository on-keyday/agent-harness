package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A-D33: the table's About cell and the `d` popup's kind line mark a run's
// PTY where it is not the mode's default, as `ls` does.
func TestPtyMarkOnRowAndDetail(t *testing.T) {
	pipe := progressTask(protocol.TaskKind_Interactive, "", 0, false, "")
	pipe.SetNoPty(true)
	if got := aboutCell(pipe); got != "<interactive+pipe>" {
		t.Errorf("no-PTY About = %q", got)
	}
	if d := formatTaskDetail(pipe, nil); !strings.Contains(d, "kind:          interactive+pipe\n") {
		t.Errorf("no-PTY detail kind line:\n%s", d)
	}
	ptyOneshot := progressTask(protocol.TaskKind_Oneshot, "", 0, false, "")
	if got := aboutCell(ptyOneshot); got != "(oneshot+pty) the original ask" {
		t.Errorf("PTY one-shot About = %q", got)
	}
	if d := formatTaskDetail(ptyOneshot, nil); !strings.Contains(d, "kind:          oneshot+pty\n") {
		t.Errorf("PTY one-shot detail kind line:\n%s", d)
	}
}

// The choice is per run, and the TUI updates a known row from events alone.
func TestApplyEventNoPty(t *testing.T) {
	a := New(Config{})
	var ti protocol.TaskInfo
	ev := protocol.TaskStatusEvent{TaskStatus: protocol.TaskStatus_Running}
	ev.SetNoPty(true)
	a.applyEventProgress(&ti, "x", ev)
	if !ti.NoPty() {
		t.Fatal("event no_pty=true did not reach the row")
	}
	ev.SetNoPty(false)
	a.applyEventProgress(&ti, "x", ev)
	if ti.NoPty() {
		t.Fatal("event no_pty=false did not clear the row")
	}
}

// A-D33: the background form's PTY field, beside the record field and read
// the same way: default sends nothing and the server takes the mode's default.
func TestBgFormPtyField(t *testing.T) {
	for _, c := range []struct {
		typed    string
		set, pty bool
		bad      bool
	}{
		{"", false, false, false},
		{"default", false, false, false},
		{"on", true, true, false},
		{"off", true, false, false},
		{"maybe", false, false, true},
	} {
		f := NewBgForm()
		f.OpenNew("/r", "claude")
		f.prompt.SetValue("triage")
		f.fields[bgFieldPty].SetValue(c.typed)
		flags, _, err := f.Values()
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "pty") {
				t.Fatalf("%q: err %v, want a refusal naming pty", c.typed, err)
			}
			continue
		}
		if err != nil || flags.PtySet != c.set || flags.Pty != c.pty {
			t.Fatalf("%q: set=%v pty=%v err=%v, want set=%v pty=%v", c.typed, flags.PtySet, flags.Pty, err, c.set, c.pty)
		}
	}
}
