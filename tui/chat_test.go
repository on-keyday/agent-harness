package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// transcript is the plain text of every line, which is what assertions want:
// the model keeps text and style apart so wrapping never measures an escape.
func transcript(m ChatModel) string {
	var b strings.Builder
	for _, l := range m.lines {
		b.WriteString(l.text)
		b.WriteString("\n")
	}
	return b.String()
}

func streamLineOf(t *testing.T, s string) cli.StreamLine {
	t.Helper()
	m, err := streamagent.DecodeMsg([]byte(s))
	if err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return cli.StreamLine{Msg: m, Raw: []byte(s), Decoded: true}
}

func openChat(t *testing.T) ChatModel {
	t.Helper()
	m := NewChatModel()
	m.open = true
	m.taskID = "abcdef0123456789abcdef0123456789"
	m.width, m.height = 100, 30
	m.restoreInput()
	return m
}

func TestChatFoldsEventsIntoTheTranscript(t *testing.T) {
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"event","event":{"kind":"tool_start","tool":"Bash","args":"ls"}}`))
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"event","event":{"kind":"text","text":"done"}}`))

	body := transcript(m)
	if !strings.Contains(body, "Bash") {
		t.Errorf("tool line missing from the transcript: %q", body)
	}
	if !strings.Contains(body, "done") {
		t.Errorf("answer missing from the transcript: %q", body)
	}
}

func TestChatShowsANonProtocolLineRatherThanDroppingIt(t *testing.T) {
	// `session send` can lawfully put one there. A follower that hides it
	// cannot explain what the adapter does next.
	m := openChat(t)
	m.applyLine(cli.StreamLine{Raw: []byte("hello, not json"), Decoded: false})
	body := transcript(m)
	if !strings.Contains(body, "hello, not json") {
		t.Errorf("raw line dropped: %q", body)
	}
}

func TestChatHoldsAPendingRequestWithItsInput(t *testing.T) {
	// The whole reason the chat reads the STREAM: RenderText drops a request's
	// Input, and Input is what the operator decides on.
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Write","input":{"file_path":"/tmp/x","content":"hi"}}}`))

	if m.pending == nil {
		t.Fatal("a request must become pending STATE, not just a transcript line")
	}
	if m.pending.ID != "req-ab12-1" {
		t.Errorf("pending id = %q", m.pending.ID)
	}
	if !strings.Contains(string(m.pending.Input), "file_path") {
		t.Errorf("Input was not retained: %q", m.pending.Input)
	}
	if !strings.Contains(m.View(), "/tmp/x") {
		t.Error("the approval view must show what is about to be written")
	}
}

func TestChatAllowBuildsTheResponseAndClearsPending(t *testing.T) {
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Write"}}`))

	sent := m.buildApproval(streamagent.BehaviorAllow, "")
	if sent == nil || sent.Response == nil {
		t.Fatal("allow must build a response message")
	}
	if sent.Response.ID != "req-ab12-1" {
		t.Errorf("the answer must carry the request id (the staleness guard), got %q", sent.Response.ID)
	}
	if sent.Response.Behavior != streamagent.BehaviorAllow {
		t.Errorf("behavior = %q", sent.Response.Behavior)
	}
	if m.pending != nil {
		t.Error("pending must clear once answered")
	}
}

func TestChatDenyCarriesTheReasonToTheAgent(t *testing.T) {
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Bash"}}`))

	sent := m.buildApproval(streamagent.BehaviorDeny, "use the Makefile instead")
	if sent.Response.Message != "use the Makefile instead" {
		t.Errorf("the deny reason was lost: %+v", sent.Response)
	}
}

func TestChatBuildApprovalWithNothingPendingIsANoop(t *testing.T) {
	m := openChat(t)
	if got := m.buildApproval(streamagent.BehaviorAllow, ""); got != nil {
		t.Errorf("answering nothing must build nothing, got %+v", got)
	}
}

func TestChatAcceptSuggestionRidesTheAllow(t *testing.T) {
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Write","suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}`))
	sent := m.buildSuggestion(0)
	if sent == nil || sent.Response == nil {
		t.Fatal("a suggestion must build a response")
	}
	if sent.Response.AcceptSuggestion == nil || *sent.Response.AcceptSuggestion != 0 {
		t.Fatalf("AcceptSuggestion not set: %+v", sent.Response)
	}
	if sent.Response.Behavior != streamagent.BehaviorAllow {
		t.Errorf("accepting a suggestion also allows this call, got %q", sent.Response.Behavior)
	}
}

func TestChatSuggestionOutOfRangeIsANoop(t *testing.T) {
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Write"}}`))
	if got := m.buildSuggestion(3); got != nil {
		t.Errorf("a suggestion index the request does not have must build nothing, got %+v", got)
	}
	if m.pending == nil {
		t.Error("and it must NOT clear the pending request")
	}
}

func TestChatRestoresThePromptAfterTheDenyEditor(t *testing.T) {
	// kscale's recorded trap: a sub-mode that replaces the input and does not
	// restore it leaves the wrong prompt stuck forever.
	m := openChat(t)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Bash"}}`))
	m.enterDenyReason()
	if m.mode != chatModeDenyReason {
		t.Fatal("deny should switch modes")
	}
	if strings.Contains(m.input.Prompt, "you") {
		t.Error("the deny editor should not still say `you`")
	}
	m.cancelSubMode()
	if m.mode != chatModeNormal {
		t.Error("esc must return to the normal mode")
	}
	if !strings.Contains(m.input.Prompt, "you") {
		t.Errorf("prompt not restored: %q", m.input.Prompt)
	}
	if m.pending == nil {
		t.Error("cancelling the reason editor must not answer the request")
	}
}

func TestChatTranscriptIsBounded(t *testing.T) {
	m := openChat(t)
	for i := 0; i < chatLineLimit+50; i++ {
		m.appendLine("line")
	}
	if len(m.lines) > chatLineLimit {
		t.Errorf("transcript grew to %d, want <= %d", len(m.lines), chatLineLimit)
	}
}

func TestChatIgnoresLinesFromAnotherTask(t *testing.T) {
	// Closing one chat and opening another leaves the first pump briefly alive;
	// its lines must not land in the new transcript.
	m := openChat(t)
	before := len(m.lines)
	m.Update(ChatLineMsg{TaskID: "0000000000000000000000000000ffff",
		Line: streamLineOf(t, `{"v":1,"kind":"event","event":{"kind":"text","text":"stale"}}`)})
	if len(m.lines) != before {
		t.Errorf("a line for another task was folded in: %q", transcript(m))
	}
}

// A transcript line is a LOGICAL line: it can hold embedded newlines (an
// agent's multi-paragraph answer) and can be far wider than the terminal. The
// view must clip by the rows a terminal will actually show, or it overflows
// the frame — which is what it did until the wrap step was added.
func TestChatViewFitsTheTerminal(t *testing.T) {
	m := openChat(t)
	m.width, m.height = 60, 20
	m.appendLine(strings.Repeat("x", 500))
	m.appendLine("para one\npara two\npara three\npara four\npara five")
	for i := 0; i < 40; i++ {
		m.appendLine("short")
	}
	if got := renderedRows(m.View(), m.width); got > m.height {
		t.Errorf("view occupies %d terminal rows in a %d-row terminal", got, m.height)
	}
}

// renderedRows counts the rows a TERMINAL will occupy, which is not the number
// of newlines the view wrote: a line wider than the frame wraps. Measured with
// ceil-division on display width rather than through chatRows, so a view that
// clips with chatRows cannot agree with this by construction — the first
// version of this check counted newlines, passed with the defect restored, and
// proved nothing.
func renderedRows(view string, width int) int {
	if width < 1 {
		width = 1
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
		w := lipgloss.Width(l)
		if w == 0 {
			n++
			continue
		}
		n += (w + width - 1) / width
	}
	return n
}

func TestChatViewFitsWithAPendingApproval(t *testing.T) {
	// The approval block is appended to the same rows, so it has to be counted
	// in the same budget — a big tool input must not push the prompt off.
	m := openChat(t)
	m.width, m.height = 60, 20
	big := strings.Repeat("y", 400)
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-a-1","tool":"Write","input":{"content":"`+big+`"}}}`))
	if got := renderedRows(m.View(), m.width); got > m.height {
		t.Errorf("view with a pending approval occupies %d rows in %d", got, m.height)
	}
}

func TestChatScrollKeysMoveTheWindow(t *testing.T) {
	// m.scroll existed and was applied by the view, but no key ever changed it,
	// so the scrollback was unreachable.
	m := openChat(t)
	m.width, m.height = 60, 20
	for i := 0; i < 100; i++ {
		m.appendLine(fmt.Sprintf("line %d", i))
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.scroll == 0 {
		t.Fatal("PgUp did not scroll")
	}
	up := m.scroll
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if m.scroll >= up {
		t.Errorf("PgDown did not come back: %d -> %d", up, m.scroll)
	}
	// And it must not run off the top of a short transcript.
	for i := 0; i < 50; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	}
	if m.scroll >= len(chatRows(m.lines, 56)) {
		t.Errorf("scroll ran past the transcript: %d", m.scroll)
	}
}

func TestChatScrollStaysAnchoredWhileStreaming(t *testing.T) {
	// Scrolled up, arriving lines must not yank the view down — and the
	// adjustment is in PHYSICAL rows, since that is what scroll counts.
	m := openChat(t)
	m.width, m.height = 60, 20
	for i := 0; i < 60; i++ {
		m.appendLine("short")
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	before := m.scroll
	m.appendLine(strings.Repeat("z", 200)) // wraps to several rows
	if m.scroll <= before {
		t.Errorf("a wrapped arrival moved the window: %d -> %d", before, m.scroll)
	}
}
