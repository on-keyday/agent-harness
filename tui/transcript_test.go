package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Agent transcripts in the d popup (pillar B): listed under the records, with
// the agent, the session id, both times and the size, and the note naming a
// store that could not be read.
func TestDetailShowsTranscriptsWithAgentTimeAndSize(t *testing.T) {
	a := bgTaskApp(t, protocol.TaskStatus_Detached, protocol.RunMode_Background)
	a.focus = focusTasks
	id := a.tasks.SelectedID()
	if _, ok := a.onDetail(keyRune('d')); !ok || !a.detail.IsOpen() {
		t.Fatal("d did not open the detail popup")
	}
	if !strings.Contains(a.detail.body, "transcripts:") {
		t.Fatalf("the popup does not say transcripts are coming:\n%s", a.detail.body)
	}
	var s protocol.TranscriptSession
	s.SetAgent([]byte("claude"))
	s.SetId([]byte("0f3c2a1e-sess"))
	s.CreatedNs = uint64(time.Date(2026, 11, 14, 9, 0, 0, 0, time.Local).UnixNano())
	s.UpdatedNs = uint64(time.Date(2026, 11, 14, 10, 30, 0, 0, time.Local).UnixNano())
	s.Size = 3 << 20
	s.SetSizeKnown(true)
	m, _ := a.Update(RecordListMsg{TaskID: id})
	a = m.(*App)
	m, _ = a.Update(TranscriptListMsg{TaskID: id, Sessions: []protocol.TranscriptSession{s}, Note: "unavailable: agy: not built"})
	a = m.(*App)
	body := a.detail.body
	for _, want := range []string{"claude", "0f3c2a1e-sess", "2026-11-14 09:00:00", "2026-11-14 10:30:00", "3.0MiB", "agy: not built"} {
		if !strings.Contains(body, want) {
			t.Fatalf("transcripts block lacks %q:\n%s", want, body)
		}
	}
	if r, tr := strings.Index(body, "records:"), strings.Index(body, "transcripts:"); r < 0 || tr < r {
		t.Fatalf("records must stay, above transcripts:\n%s", body)
	}
	// A late record list does not erase the transcripts, and vice versa.
	m, _ = a.Update(RecordListMsg{TaskID: id})
	if !strings.Contains(m.(*App).detail.body, "0f3c2a1e-sess") {
		t.Fatal("a record list replaced the transcripts block")
	}
}

func TestDetailTranscriptListError(t *testing.T) {
	a := bgTaskApp(t, protocol.TaskStatus_Detached, protocol.RunMode_Background)
	a.focus = focusTasks
	id := a.tasks.SelectedID()
	a.onDetail(keyRune('d'))
	m, _ := a.Update(TranscriptListMsg{TaskID: id, Err: errors.New("runner_offline: boom")})
	a = m.(*App)
	if !strings.Contains(a.detail.body, "transcripts:") || !strings.Contains(a.detail.body, "boom") {
		t.Fatalf("a failed transcript list is not shown:\n%s", a.detail.body)
	}
	// An answer for a task the popup no longer shows is dropped.
	m, _ = a.Update(TranscriptListMsg{TaskID: strings.Repeat("cd", 16), Err: errors.New("other task")})
	if strings.Contains(m.(*App).detail.body, "other task") {
		t.Fatal("another task's answer reached the popup")
	}
	// With nothing recorded the block says none, never a blank.
	m, _ = a.Update(TranscriptListMsg{TaskID: id})
	if !strings.Contains(m.(*App).detail.body, "transcripts:   none") {
		t.Fatalf("an empty list is not shown as none:\n%s", m.(*App).detail.body)
	}
}

// A transcript's bytes written into the results pane would draw into the TUI.
func TestTuiTranscriptGetNeedsOutputFile(t *testing.T) {
	a := New(Config{})
	if cmd := (tuiVerbs{a}).TranscriptGet(verb.TranscriptGetAction{TaskID: strings.Repeat("ab", 16), Session: "s"}); cmd != nil {
		t.Fatal("transcript get with no -o ran")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "-o") {
		t.Fatalf("no explanation naming -o: %v", a.cmdresult.lines)
	}
}
