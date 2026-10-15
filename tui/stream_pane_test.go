package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestGridTilesStreamSessions(t *testing.T) {
	tasks := []protocol.TaskInfo{
		{Kind: protocol.TaskKind_Interactive, Status: protocol.TaskStatus_Running},
		{Kind: protocol.TaskKind_Stream, Status: protocol.TaskStatus_Detached},
		{Kind: protocol.TaskKind_Stream, Status: protocol.TaskStatus_Succeeded},
		{Kind: protocol.TaskKind_Oneshot, Status: protocol.TaskStatus_Running},
	}
	got := gridLiveTasks(tasks)
	if len(got) != 2 {
		t.Fatalf("tiled %d tasks, want the live interactive and the live stream one", len(got))
	}
}

// A stream pane in input mode takes the chat's keys: letters answer a pending
// approval, Esc backs out of a sub-editor, and the badge tracks the request.
func TestStreamPaneDrivesWithTheChatsKeys(t *testing.T) {
	p := NewStreamPane("abcdef0123456789abcdef0123456789")
	p.SetInputActive(true)
	p.d.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-p","tool":"Bash"}}`))
	if !strings.Contains(p.Badge(), "⚑") {
		t.Fatalf("badge %q does not mark the pending request", p.Badge())
	}

	p.Key(keyRunes("d"))
	if p.d.mode != chatModeDenyReason {
		t.Fatalf("d did not open the deny reason (mode %v)", p.d.mode)
	}
	p.Key(tea.KeyMsg{Type: tea.KeyEsc})
	if p.d.mode != chatModeNormal || p.d.pending == nil {
		t.Fatal("esc in the reason editor must back out without answering")
	}

	// No session in a unit test, so the send is reported rather than made;
	// what matters is that `a` was read as the decision, not typed.
	p.Key(keyRunes("a"))
	if p.d.pending != nil {
		t.Fatal("a did not answer the approval")
	}
	if p.d.input.Value() != "" {
		t.Fatalf("a was typed into the input: %q", p.d.input.Value())
	}
	if strings.Contains(p.Badge(), "⚑") {
		t.Fatal("badge still marks a request that was answered")
	}

	p.Key(keyRunes("hello"))
	if p.d.input.Value() != "hello" {
		t.Fatalf("draft %q, want hello", p.d.input.Value())
	}
}

// Render never exceeds the cell: the grid's lipgloss box wraps anything wider,
// which pushes the whole grid past the terminal.
func TestStreamPaneRenderFitsTheCell(t *testing.T) {
	p := NewStreamPane("abcdef0123456789abcdef0123456789")
	p.SetInputActive(true)
	for i := 0; i < 30; i++ {
		p.d.applyLine(streamLineOf(t, `{"v":1,"kind":"event","event":{"kind":"text","text":"日本語の長い行が折り返されることを確かめるための文章です"}}`))
	}
	p.d.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-r","tool":"Write","input":{"path":"/tmp/x","content":"a very long body that will not fit"}}}`))
	p.Key(keyRunes("d")) // the widest prompt: "deny reason ▶ "
	const w, h = 24, 8
	out := p.Render(w, h)
	rows := strings.Split(out, "\n")
	if len(rows) > h {
		t.Fatalf("%d rows, want ≤ %d", len(rows), h)
	}
	for i, r := range rows {
		if got := runewidth.StringWidth(stripANSI(r)); got > w {
			t.Errorf("row %d is %d wide, want ≤ %d: %q", i, got, w, stripANSI(r))
		}
	}
}

// A draft typed after the first render shows whole: the input's scroll is
// computed against the width it holds at keystroke time, which the pane only
// learns when it is rendered.
func TestStreamPaneDraftIsNotCutToTheInitialWidth(t *testing.T) {
	p := NewStreamPane("abcdef0123456789abcdef0123456789")
	p.SetInputActive(true)
	p.Render(100, 10)
	p.Key(keyRunes("reply with exactly: GRID-OK"))
	if out := stripANSI(p.Render(100, 10)); !strings.Contains(out, "reply with exactly: GRID-OK") {
		t.Fatalf("draft cut short:\n%s", out)
	}
}
