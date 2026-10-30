package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// BoardMembershipMsg carries the result of DoBoardMembership.
type BoardMembershipMsg struct {
	Op          protocol.BoardMembershipOp
	Task, Topic string
	Err         error
}

// Label is the status line for a request that succeeded.
func (m BoardMembershipMsg) Label() string {
	return fmt.Sprintf("%s %s %s: ok", m.Op, shortHex(m.Task), m.Topic)
}

// DoBoardMembership subscribes / joins / unsubscribes / leaves task on topic
// over the long-lived client, as `board <op> <task> <topic>` does.
func DoBoardMembership(c *cli.Client, task, topic string, op protocol.BoardMembershipOp) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := c.BoardMembership(ctx, task, topic, op)
		return BoardMembershipMsg{Op: op, Task: task, Topic: topic, Err: err}
	}
}

func shortHex(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// boardMemberEdit is the task-id editor S / J open in the subscribers view.
type boardMemberEdit struct {
	input textinput.Model
	op    protocol.BoardMembershipOp
	topic string
}

// BeginMemberEdit opens the task-id editor for op on the open topic.
func (m *BoardModal) BeginMemberEdit(op protocol.BoardMembershipOp) {
	if m.curTopic == "" {
		return
	}
	prompt := op.String() + " a task to " + m.curTopic + " ▶ "
	ti := textinput.New()
	ti.Prompt = prompt
	ti.Placeholder = "task id or a unique prefix"
	ti.Width = m.content.Width - len(prompt)
	ti.Focus()
	m.member = &boardMemberEdit{input: ti, op: op, topic: m.curTopic}
}

// EditingMember reports whether the task-id editor is open; the App routes
// every key here while it is.
func (m *BoardModal) EditingMember() bool { return m.member != nil }

// HandleMemberKey feeds one key to the task-id editor. submit is true when
// Enter submitted a non-blank prefix, which the App resolves against its task
// list; the editor is closed then.
func (m *BoardModal) HandleMemberKey(k tea.KeyMsg) (submit bool, op protocol.BoardMembershipOp, prefix, topic string) {
	if m.member == nil {
		return false, 0, "", ""
	}
	switch k.Type {
	case tea.KeyEsc:
		m.member = nil
		return false, 0, "", ""
	case tea.KeyEnter:
		prefix = strings.TrimSpace(m.member.input.Value())
		if prefix == "" {
			return false, 0, "", ""
		}
		op, topic = m.member.op, m.member.topic
		m.member = nil
		return true, op, prefix, topic
	}
	m.member.input, _ = m.member.input.Update(k)
	return false, 0, "", ""
}

func (m *BoardModal) memberEditView() string {
	what := "subscribed only: it may read, nothing more"
	if m.member.op == protocol.BoardMembershipOp_Join {
		what = "a member: subscribed, may publish here, answers to its posts stay here"
	}
	return m.member.input.View() + "\n" + MutedStyle.Render("enter: "+what+" · esc cancels")
}

// MembershipTarget is the task U / L act on: the highlighted line's task. L
// on a task that is not a member says so on the status line instead.
func (m *BoardModal) MembershipTarget(op protocol.BoardMembershipOp) (string, bool) {
	if m.subCursor < 0 || m.subCursor >= len(m.subItems) {
		return "", false
	}
	r := m.subRows[m.subItems[m.subCursor].row]
	if op == protocol.BoardMembershipOp_Leave && !joinedOn(r, m.curTopic) {
		m.status = fmt.Sprintf("leave: %s is not a member of %s — %s unsubscribes it",
			shortHex(r.TaskHex), m.curTopic, modalKeys.BoardUnsubscribeTask)
		return "", false
	}
	return r.TaskHex, true
}

func joinedOn(r cli.BoardSubscriberRow, topic string) bool {
	for _, p := range r.Patterns {
		if p.Name == topic {
			return p.Joined
		}
	}
	return false
}

// allMembers reports whether subs is non-empty and every one of them is a
// member of topic: then every task that could answer may answer on it.
func allMembers(subs []cli.BoardSubscriberRow, topic string) bool {
	for _, r := range subs {
		if !joinedOn(r, topic) {
			return false
		}
	}
	return len(subs) > 0
}

// hasMember reports whether any of subs is a member of topic.
func hasMember(subs []cli.BoardSubscriberRow, topic string) bool {
	for _, r := range subs {
		if joinedOn(r, topic) {
			return true
		}
	}
	return false
}

// patternLabel is a subscribed topic as the subscribers view names it: a
// member's with joined:, the spelling `board subscribers` prints.
func patternLabel(p cli.BoardSubscriberPattern) string {
	if p.Joined {
		return "joined:" + p.Name
	}
	return p.Name
}

// nextReplyTo is ctrl+r in the composer: the next route in the cycle. The
// topic itself is in it only when the topic has a member to read the answers.
func nextReplyTo(p cli.BoardSendParams, subs []cli.BoardSubscriberRow) string {
	routes := []string{agentboard.OperatorTopic, ""}
	if p.Topic != "" && hasMember(subs, p.Topic) {
		routes = append([]string{p.Topic}, routes...)
	}
	for i, r := range routes {
		if r == p.ReplyTo {
			return routes[(i+1)%len(routes)]
		}
	}
	return routes[0]
}
