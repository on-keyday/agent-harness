package tui

import (
	"context"
	"strconv"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
)

// BoardSendMsg carries the result of DoBoardSend.
type BoardSendMsg struct {
	Params cli.BoardSendParams
	Result cli.BoardSendResult
	Err    error
}

// BoardWakeMsg carries the result of DoBoardWake.
type BoardWakeMsg struct {
	Topic string
	Woken int
	Err   error
}

// DoBoardSend publishes as the operator over the long-lived client. Mirrors
// DoBoardRetract: same client, same timeout.
func DoBoardSend(c *cli.Client, p cli.BoardSendParams, body string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := c.BoardSend(ctx, p, []byte(body))
		return BoardSendMsg{Params: p, Result: res, Err: err}
	}
}

// DoBoardWake wakes a topic's subscribers over the long-lived client.
func DoBoardWake(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		n, err := c.BoardWake(ctx, topic)
		return BoardWakeMsg{Topic: topic, Woken: n, Err: err}
	}
}

// boardCompose is the one-line editor the message view swaps in for `m`
// (a new message on the open topic) and `a` (a reply to the selected one).
// The modal owns its keys while it is open, the way the chat modal's deny-reason
// editor does, so typing a letter never reaches the board's own bindings.
type boardCompose struct {
	input  textinput.Model
	params cli.BoardSendParams
}

// BeginCompose opens the editor for a new message on the open topic. Like a
// reply, it asks for answers on chat.operator (operator, 2026-10-05): without
// a destination the agent's answer would be refused (no_reply_route), which
// reads from here as an agent that never answered. ctrl+r drops it for a note
// that wants no answer on the board.
func (m *BoardModal) BeginCompose() {
	m.openCompose(cli.BoardSendParams{Topic: m.curTopic, ReplyTo: agentboard.OperatorTopic},
		"send to "+m.curTopic+" ▶ ")
}

// BeginReply opens the editor for a reply to seq, asking for answers on
// chat.operator for BeginCompose's reason; the parent's own topic would not
// do, because it would wake the agent with its own answer.
func (m *BoardModal) BeginReply(seq uint64) {
	if seq == 0 {
		return
	}
	m.openCompose(cli.BoardSendParams{InReplyTo: seq, ReplyTo: agentboard.OperatorTopic},
		"reply to #"+strconv.FormatUint(seq, 10)+" ▶ ")
}

func (m *BoardModal) openCompose(p cli.BoardSendParams, prompt string) {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.Placeholder = "message as the operator"
	ti.Width = m.content.Width - len(prompt)
	ti.Focus()
	m.compose = &boardCompose{input: ti, params: p}
}

// Composing reports whether the editor is open; the App routes every key here
// while it is.
func (m *BoardModal) Composing() bool { return m.compose != nil }

// EndCompose closes the editor without sending.
func (m *BoardModal) EndCompose() { m.compose = nil }

// HandleComposeKey feeds one key to the editor. send is true when Enter
// submitted a non-empty body; the editor is closed then, and p and body are
// what to publish. Tab turns the wake off and on; Esc leaves without sending.
func (m *BoardModal) HandleComposeKey(k tea.KeyMsg) (send bool, p cli.BoardSendParams, body string) {
	if m.compose == nil {
		return false, p, ""
	}
	switch k.Type {
	case tea.KeyEsc:
		m.EndCompose()
		return false, p, ""
	case tea.KeyTab:
		m.compose.params.NoWake = !m.compose.params.NoWake
		return false, p, ""
	case tea.KeyCtrlT:
		// keep: --no-retire-on-reply. Off by default, as on the CLI.
		m.compose.params.NoRetireOnReply = !m.compose.params.NoRetireOnReply
		return false, p, ""
	case tea.KeyCtrlR:
		if m.compose.params.ReplyTo == "" {
			m.compose.params.ReplyTo = agentboard.OperatorTopic
		} else {
			m.compose.params.ReplyTo = ""
		}
		return false, p, ""
	case tea.KeyEnter:
		body = m.compose.input.Value()
		if body == "" {
			return false, p, ""
		}
		p = m.compose.params
		m.EndCompose()
		return true, p, body
	}
	m.compose.input, _ = m.compose.input.Update(k)
	return false, p, ""
}

// composeView is the editor line and its hint, drawn under the message list.
func (m *BoardModal) composeView() string {
	wake := "on"
	if m.compose.params.NoWake {
		wake = "off"
	}
	replyTo := m.compose.params.ReplyTo
	if replyTo == "" {
		replyTo = "none"
	}
	keep := "off"
	if m.compose.params.NoRetireOnReply {
		keep = "on"
	}
	return m.compose.input.View() + "\n" +
		MutedStyle.Render("enter sends · tab: wake: "+wake+" · ctrl+r: reply-to: "+replyTo+
			" · ctrl+t: keep: "+keep+" · esc cancels")
}

// BeginReplyToLatest opens the reply editor on the newest message of the
// highlighted conversation, for the chain views, which have a cursor per
// conversation but none per message. The newest is the one a reply is usually
// for; an earlier one is answered from the topic view. False when nothing is
// highlighted.
func (m *BoardModal) BeginReplyToLatest() bool {
	conv, ok := m.selectedConv()
	if !ok || len(conv.Rows) == 0 {
		return false
	}
	latest := conv.Rows[0].Msg
	for _, r := range conv.Rows[1:] {
		if r.Msg.ReceivedAtMs > latest.ReceivedAtMs ||
			(r.Msg.ReceivedAtMs == latest.ReceivedAtMs && r.Msg.Seq > latest.Seq) {
			latest = r.Msg
		}
	}
	m.BeginReply(latest.Seq)
	return true
}
