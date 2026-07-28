package tui

import (
	"strings"
	"testing"
)

// A superseded history response must be discarded. Before the generation
// guard, a second followTask for the same task folded a second full copy of
// the log file into the pane — pressing Enter twice on a slow fetch showed
// everything twice.
func TestStaleLogHistoryIsDropped(t *testing.T) {
	a := New(Config{})
	taskID := "0123456789abcdef0123456789abcdef"
	a.logs.Reset(taskID)
	a.logsGen = 2 // followTask ran twice; only generation 2 is current

	a.Update(LogHistoryMsg{TaskID: taskID, Content: []byte("HISTORY\n"), Found: true, Gen: 1})
	a.Update(LogHistoryMsg{TaskID: taskID, Content: []byte("HISTORY\n"), Found: true, Gen: 2})

	body := strings.Join(a.logs.lines, "")
	if n := strings.Count(body, "HISTORY"); n != 1 {
		t.Fatalf("history appears %d times, want exactly 1:\n%s", n, body)
	}
}

func TestCurrentLogHistoryIsApplied(t *testing.T) {
	a := New(Config{})
	taskID := "0123456789abcdef0123456789abcdef"
	a.logs.Reset(taskID)
	a.logsGen = 1

	a.Update(LogHistoryMsg{TaskID: taskID, Content: []byte("HISTORY\n"), Found: true, Gen: 1})

	if !strings.Contains(strings.Join(a.logs.lines, ""), "HISTORY") {
		t.Fatal("matching-generation history was dropped")
	}
}
