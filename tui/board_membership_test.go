package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	memberTask = "aaaa1111000000000000000000000001"
	plainTask  = "aaaa2222000000000000000000000002"
)

// subscribersApp is an App with the board modal on the subscribers view of
// grp: memberTask joined, plainTask only subscribed.
func subscribersApp(t *testing.T) *App {
	t.Helper()
	a := &App{boardModal: NewBoardModal(), tasksByID: map[string]protocol.TaskInfo{
		memberTask: {}, plainTask: {},
	}}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplySubscribers("grp", []cli.BoardSubscriberRow{
		{TaskHex: memberTask, Patterns: []cli.BoardSubscriberPattern{{Name: "grp", Joined: true}}},
		{TaskHex: plainTask, Patterns: []cli.BoardSubscriberPattern{{Name: "grp"}}},
	})
	return a
}

func key(r string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)} }

func TestApp_BoardJoinKey(t *testing.T) {
	a := subscribersApp(t)
	a.updateKey(key(modalKeys.BoardJoinTask))
	if !a.boardModal.EditingMember() {
		t.Fatal("J did not open the task editor")
	}
	if v := a.boardModal.View(); !strings.Contains(v, "join") || !strings.Contains(v, "grp") {
		t.Errorf("the editor does not say what it does: %q", v)
	}
	a.updateKey(key("aaaa2"))
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("J + a unique prefix + Enter produced no request")
	}
	if a.boardModal.EditingMember() {
		t.Error("still editing after a submitted request")
	}
}

func TestApp_BoardJoinAmbiguousPrefix(t *testing.T) {
	a := subscribersApp(t)
	a.updateKey(key(modalKeys.BoardSubscribeTask))
	a.updateKey(key("aaaa"))
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("an ambiguous prefix produced a request")
	}
	if !strings.Contains(a.boardModal.View(), "ambiguous") {
		t.Error("the ambiguous prefix is not reported on the status line")
	}
}

func TestApp_BoardLeaveKey(t *testing.T) {
	a := subscribersApp(t)
	// The cursor starts on memberTask's line.
	if _, cmd := a.updateKey(key(modalKeys.BoardLeaveTask)); cmd == nil {
		t.Fatal("L on a member's line produced no request")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyDown})
	if _, cmd := a.updateKey(key(modalKeys.BoardLeaveTask)); cmd != nil {
		t.Fatal("L on a non-member's line produced a request")
	}
	if !strings.Contains(a.boardModal.View(), "not a member") {
		t.Error("L on a non-member does not say why nothing happened")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardUnsubscribeTask)); cmd == nil {
		t.Fatal("U on a subscriber's line produced no request")
	}
}

func TestBoardSubscribersView_MarksJoined(t *testing.T) {
	a := subscribersApp(t)
	v := a.boardModal.View()
	if !strings.Contains(v, "joined:grp") || strings.Count(v, "joined:") != 1 {
		t.Errorf("subscribers view should mark the one member joined:\n%s", v)
	}
}

func TestBoardCompose_ReplyToTopicWithMember(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(160, 45)
	member := []cli.BoardSubscriberRow{{TaskHex: memberTask, Patterns: []cli.BoardSubscriberPattern{{Name: "grp", Joined: true}}}}
	m.ApplyMessages("grp", nil, member, true)
	m.BeginCompose()
	if got := m.compose.params.ReplyTo; got != "grp" {
		t.Errorf("compose on a topic with a member: reply-to %q, want grp", got)
	}
	m.EndCompose()
	// A plain subscriber beside the member could not answer on the topic (it
	// may not publish there), so the default stays chat.operator.
	mixed := append(member, cli.BoardSubscriberRow{TaskHex: plainTask, Patterns: []cli.BoardSubscriberPattern{{Name: "grp"}}})
	m.ApplyMessages("grp", nil, mixed, true)
	m.BeginCompose()
	if got := m.compose.params.ReplyTo; got != agentboard.OperatorTopic {
		t.Errorf("compose on a topic with a plain subscriber: reply-to %q, want %s", got, agentboard.OperatorTopic)
	}
	m.EndCompose()
	m.ApplyMessages("other", nil, nil, true)
	m.BeginCompose()
	if got := m.compose.params.ReplyTo; got != agentboard.OperatorTopic {
		t.Errorf("compose on a topic without members: reply-to %q, want %s", got, agentboard.OperatorTopic)
	}
}

// At 80 columns the topics list runs past the box; the membership of the
// open topic is read before it, so it is never what gets cut.
func TestBoardSubscribersView_JoinedSurvivesNarrowWidth(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(80, 24)
	m.ApplySubscribers("grp", []cli.BoardSubscriberRow{{TaskHex: memberTask, Hostname: "gmkhost", AgentProfile: "bash",
		Patterns: []cli.BoardSubscriberPattern{{Name: "chat.aaaa1111"}, {Name: "grp", Joined: true}}}})
	for _, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, "aaaa1111 ") {
			if i := strings.Index(line, "joined"); i < 0 || lipgloss.Width(line[:i]) > 70 {
				t.Errorf("the task line does not mark the membership within the box: %q", line)
			}
			return
		}
	}
	t.Fatal("no task line")
}
