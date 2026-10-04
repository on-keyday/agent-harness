package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
)

func boardModalOnTopic(t *testing.T) BoardModal {
	t.Helper()
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 30)
	m.ApplyMessages("chat.abcd1234", []cli.BoardMessage{
		{Seq: 42, FromTaskHex: "abcd1234abcd1234abcd1234abcd1234", SenderKind: "agent", ReceivedAtMs: 1_000, Payload: []byte("q")},
	}, nil, true)
	return m
}

func typeInto(m *BoardModal, s string) {
	for _, r := range s {
		m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestBoardModal_MessagesFooterNamesComposeReplyWake(t *testing.T) {
	m := boardModalOnTopic(t)
	view := m.View()
	for _, want := range []string{
		modalKeys.BoardCompose + ": send", modalKeys.BoardReply + ": reply", modalKeys.BoardWake + ": wake",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("messages footer missing %q\n%s", want, view)
		}
	}
	m.PopToTopics()
	if v := m.View(); !strings.Contains(v, modalKeys.BoardWake+": wake") {
		t.Errorf("topics footer missing the wake key\n%s", v)
	}
}

func TestBoardModal_ComposeSendsOnTheOpenTopicAndWakesByDefault(t *testing.T) {
	m := boardModalOnTopic(t)
	m.BeginCompose()
	if !m.Composing() {
		t.Fatal("not composing after BeginCompose")
	}
	typeInto(&m, "hello")
	send, p, body := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !send || body != "hello" || p.Topic != "chat.abcd1234" || p.NoWake || p.InReplyTo != 0 ||
		p.ReplyTo != agentboard.OperatorTopic {
		t.Fatalf("send=%v body=%q params=%+v", send, body, p)
	}
	if m.Composing() {
		t.Error("still composing after the send")
	}
}

func TestBoardModal_ReplyPrefillsOperatorTopic(t *testing.T) {
	m := boardModalOnTopic(t)
	m.BeginReply(m.SelectedMsgSeq())
	typeInto(&m, "answer")
	send, p, _ := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !send || p.InReplyTo != 42 || p.ReplyTo != agentboard.OperatorTopic || p.Topic != "" {
		t.Fatalf("send=%v params=%+v, want in_reply_to=42 reply_to=chat.operator topic empty", send, p)
	}
}

func TestBoardModal_TabTogglesWake(t *testing.T) {
	m := boardModalOnTopic(t)
	m.BeginCompose()
	m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyTab})
	if !strings.Contains(m.View(), "wake: off") {
		t.Errorf("view does not say wake is off\n%s", m.View())
	}
	typeInto(&m, "quiet")
	_, p, _ := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !p.NoWake {
		t.Fatal("tab did not turn the wake off")
	}
}

func TestBoardModal_ComposeEscCancelsAndEmptyEnterDoesNotSend(t *testing.T) {
	m := boardModalOnTopic(t)
	m.BeginCompose()
	if send, _, _ := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter}); send {
		t.Error("an empty body was sent")
	}
	m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.Composing() {
		t.Error("esc did not leave the compose line")
	}
}

func TestBoardModal_OperatorSenderIsNamed(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 30)
	m.ApplyMessages("chat.x", []cli.BoardMessage{
		{Seq: 1, FromTaskHex: strings.Repeat("0", 32), SenderKind: "operator", ReceivedAtMs: 1_000},
	}, nil, true)
	v := m.View()
	if !strings.Contains(v, "from=operator") || strings.Contains(v, "00000000") {
		t.Fatalf("view does not name the operator:\n%s", v)
	}
}

// ctrl+r drops the reply destination, for a note that wants no answer on the
// board; pressed again it comes back.
func TestBoardModal_CtrlRTogglesTheReplyDestination(t *testing.T) {
	m := boardModalOnTopic(t)
	m.BeginCompose()
	m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !strings.Contains(m.View(), "reply-to: none") {
		t.Errorf("view does not say the reply destination is off\n%s", m.View())
	}
	typeInto(&m, "fyi")
	_, p, _ := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if p.ReplyTo != "" {
		t.Fatalf("ReplyTo = %q after ctrl+r, want empty", p.ReplyTo)
	}
	m.BeginReply(42)
	m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	typeInto(&m, "x")
	_, p, _ = m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if p.ReplyTo != agentboard.OperatorTopic {
		t.Fatalf("ReplyTo = %q after ctrl+r twice, want chat.operator", p.ReplyTo)
	}
}

// ctrl+t keeps the message on the board after its recipient answers
// (--no-retire-on-reply); off by default, like the CLI flag.
func TestBoardModal_CtrlTKeepsTheMessageAfterAReply(t *testing.T) {
	m := boardModalOnTopic(t)
	m.BeginCompose()
	if !strings.Contains(m.View(), "keep: off") {
		t.Errorf("view does not show keep: off by default\n%s", m.View())
	}
	m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !strings.Contains(m.View(), "keep: on") {
		t.Errorf("view does not say keep is on\n%s", m.View())
	}
	typeInto(&m, "standing order")
	_, p, _ := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !p.NoRetireOnReply {
		t.Fatal("ctrl+t did not set NoRetireOnReply")
	}
	m.BeginCompose()
	typeInto(&m, "x")
	_, p, _ = m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if p.NoRetireOnReply {
		t.Fatal("a fresh editor inherited keep from the last one")
	}
}

// In the chain view `a` answers the newest message of the highlighted
// conversation -- the one a reply is usually for. An earlier message is
// answered from the topic view, which has a cursor per message.
func TestBoardModal_ReplyFromChainsAnswersTheLatestMessage(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 40)
	m.ApplyChains(chainFixture(t))
	conv, ok := m.selectedConv()
	if !ok {
		t.Fatal("nothing selected")
	}
	var latest uint64
	var latestAt uint64
	for _, r := range conv.Rows {
		if r.Msg.ReceivedAtMs > latestAt || (r.Msg.ReceivedAtMs == latestAt && r.Msg.Seq > latest) {
			latest, latestAt = r.Msg.Seq, r.Msg.ReceivedAtMs
		}
	}
	for _, open := range []bool{false, true} { // from the list, and from the opened conversation
		if open {
			m.OpenSelectedConversation()
		}
		if !m.BeginReplyToLatest() {
			t.Fatalf("open=%v: BeginReplyToLatest found nothing", open)
		}
		if !strings.Contains(m.View(), "reply to #") {
			t.Errorf("open=%v: the editor is not drawn in the chain view\n%s", open, m.View())
		}
		typeInto(&m, "re")
		_, p, _ := m.HandleComposeKey(tea.KeyMsg{Type: tea.KeyEnter})
		if p.InReplyTo != latest || p.ReplyTo != agentboard.OperatorTopic {
			t.Fatalf("open=%v: params %+v, want in_reply_to=%d reply_to=chat.operator", open, p, latest)
		}
	}
}

// The send result set before the chains refresh is still on screen after it.
func TestBoardModal_SendStatusSurvivesTheChainsRefresh(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(160, 45)
	m.ApplyChains(chainFixture(t))
	m.SetStatusAfterRefresh("sent #7 (reply to #6, delivered_to=1, wake on)")
	m.ApplyChains(chainFixture(t))
	if v := m.View(); !strings.Contains(v, "sent #7") {
		t.Fatalf("status lost across the chains refresh\n%s", v)
	}
}
