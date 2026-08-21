package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

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

	body := strings.Join(m.lines, "\n")
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
	body := strings.Join(m.lines, "\n")
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
		t.Errorf("a line for another task was folded in: %q", m.lines)
	}
}
