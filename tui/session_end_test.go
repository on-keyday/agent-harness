package tui

import (
	"strings"
	"testing"
)

// A session the far side ended says why in the results pane — the reason
// came on the stream ahead of the close (exec/frame End); a detach or a
// plain close adds nothing.
func TestInteractiveDoneShowsTheEndReason(t *testing.T) {
	a := New(Config{})
	id := strings.Repeat("ab", 16)
	m, _ := a.Update(InteractiveDoneMsg{TaskID: id, EndReason: "prompt_unsupported: profile claude has no {prompt} in its interactive template"})
	lines := strings.Join(m.(*App).cmdresult.lines, "\n")
	if !strings.Contains(lines, "session ended: prompt_unsupported: profile claude has no {prompt}") {
		t.Fatalf("the reason is not in the results pane: %v", lines)
	}
	b := New(Config{})
	m, _ = b.Update(InteractiveDoneMsg{TaskID: id})
	if strings.Contains(strings.Join(m.(*App).cmdresult.lines, "\n"), "session ended:") {
		t.Fatal("a session with no End reported one")
	}
}
