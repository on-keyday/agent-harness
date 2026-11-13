package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The PTY record on the TUI (pillar A spec, A-D28).

func TestBgFormRecordField(t *testing.T) {
	for _, c := range []struct {
		typed       string
		set, record bool
		bad         bool
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
		f.fields[bgFieldRecord].SetValue(c.typed)
		flags, _, err := f.Values()
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "record") {
				t.Fatalf("%q: err %v, want a refusal naming record", c.typed, err)
			}
			continue
		}
		if err != nil || flags.RecordSet != c.set || flags.Record != c.record {
			t.Fatalf("%q: set=%v record=%v err=%v, want set=%v record=%v", c.typed, flags.RecordSet, flags.Record, err, c.set, c.record)
		}
	}
}

// The default sends nothing, so the server's --record default applies.
func TestBgFormDefaultSendsNoExt(t *testing.T) {
	f := NewBgForm()
	f.OpenNew("/r", "claude")
	f.prompt.SetValue("triage")
	flags, _, _ := f.Values()
	if flags.RecordSet {
		t.Fatal("an untouched record field says something")
	}
}

func TestDetailShowsRecordsWithSizeAndExpiry(t *testing.T) {
	a := bgTaskApp(t, protocol.TaskStatus_Detached, protocol.RunMode_Background)
	a.focus = focusTasks
	id := a.tasks.SelectedID()
	if _, ok := a.onDetail(keyRune('d')); !ok || !a.detail.IsOpen() {
		t.Fatal("d did not open the detail popup")
	}
	if !strings.Contains(a.detail.body, "records:") {
		t.Fatalf("the popup does not say records are coming:\n%s", a.detail.body)
	}
	var info protocol.RecordInfo
	copy(info.TaskId.Id[:], a.tasks.SelectedTask().Id.Id[:])
	info.Run = 2
	info.DiskBytes = 3 << 20
	info.EntryBytes = 40 << 20
	info.ExpiresNs = uint64(time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local).UnixNano())
	info.SetRecording(true)
	m, _ := a.Update(RecordListMsg{TaskID: id, Records: []protocol.RecordInfo{info}})
	a = m.(*App)
	for _, want := range []string{"#2", "3.0MiB", "2026-12-01", "recording"} {
		if !strings.Contains(a.detail.body, want) {
			t.Fatalf("records block lacks %q:\n%s", want, a.detail.body)
		}
	}
	m, _ = a.Update(RecordListMsg{TaskID: id, Err: errors.New("boom")})
	if !strings.Contains(m.(*App).detail.body, "boom") {
		t.Fatal("a failed record list is not shown")
	}
}

func TestTuiRecordGetNeedsOutputFile(t *testing.T) {
	a := New(Config{})
	if cmd := (tuiVerbs{a}).RecordGet(verb.RecordGetAction{Ref: strings.Repeat("ab", 16)}); cmd != nil {
		t.Fatal("record get with no -o ran")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "-o") {
		t.Fatalf("no explanation naming -o: %v", a.cmdresult.lines)
	}
}

// Final review I1: an attached `session new` typed on the TUI command line
// carries --record / --no-record (and the rest of the run-mode flags) like a
// detached one does.
func TestAttachedSessionNewCarriesTheRecordSwitch(t *testing.T) {
	var got []cli.RunModeFlags
	oldA, oldX := openAttachedSession, openX11Session
	defer func() { openAttachedSession, openX11Session = oldA, oldX }()
	openAttachedSession = func(c *cli.Client, repo string, sel cli.SelectorOpts, extraArgs []string, resumeTaskID string, auth Authority, capsOverride, conv, exact bool, agent string, rm cli.RunModeFlags) tea.Cmd {
		got = append(got, rm)
		return nil
	}
	openX11Session = func(c *cli.Client, repo string, sel cli.SelectorOpts, extraArgs []string, resumeTaskID string, displayN int, program *tea.Program, auth Authority, capsOverride, conv, exact bool, agent string, rm cli.RunModeFlags) tea.Cmd {
		got = append(got, rm)
		return nil
	}
	a := New(Config{DefaultRepo: "/r"})
	_ = (tuiVerbs{a}).SessionNew(verb.SpawnAction{Repo: "/r", NoRecord: true})
	_ = (tuiVerbs{a}).SessionNew(verb.SpawnAction{Repo: "/r", Record: true, X11: true})
	if len(got) != 2 {
		t.Fatalf("%d opens, want 2", len(got))
	}
	if !got[0].RecordSet || got[0].Record {
		t.Fatalf("attached --no-record: %+v, want the switch set to off", got[0])
	}
	if !got[1].RecordSet || !got[1].Record {
		t.Fatalf("x11 --record: %+v, want the switch set to on", got[1])
	}
}
