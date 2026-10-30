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
)

// BoardRetainMsg carries the result of DoBoardRetain.
type BoardRetainMsg struct {
	Topic string
	Label string // what was set: a duration, "forever", or "cleared"
	Err   error
}

// DoBoardRetain sets or clears a topic's retention over the long-lived
// client. Mirrors DoBoardPurge: same client, same timeout.
func DoBoardRetain(c *cli.Client, topic string, ttl time.Duration, forever, clear bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := c.BoardRetain(ctx, topic, ttl, forever, clear)
		label := "cleared"
		if !clear {
			label = cli.TopicRetention{TTL: ttl, Forever: forever}.Label()
		}
		return BoardRetainMsg{Topic: topic, Label: label, Err: err}
	}
}

// boardRetainEdit is the one-line editor `t` opens on a topic.
type boardRetainEdit struct {
	input textinput.Model
	topic string
}

// BeginRetain opens the retain editor for topic.
func (m *BoardModal) BeginRetain(topic string) {
	if topic == "" {
		return
	}
	if !agentboard.Retainable(topic) {
		m.status = "retain: " + topic + " cannot be retained — chat.* topics stay temporary"
		return
	}
	prompt := "retain " + topic + " ▶ "
	ti := textinput.New()
	ti.Prompt = prompt
	ti.Placeholder = "72h or forever"
	ti.Width = m.content.Width - len(prompt)
	ti.Focus()
	m.retain = &boardRetainEdit{input: ti, topic: topic}
}

// Retaining reports whether the retain editor is open; the App routes every
// key here while it is.
func (m *BoardModal) Retaining() bool { return m.retain != nil }

// HandleRetainKey feeds one key to the retain editor, which takes a period
// only (stopping is its own key). submit is true when Enter produced a valid
// period; the editor is closed then. A value that does not parse keeps the
// editor open with the reason on the status line.
func (m *BoardModal) HandleRetainKey(k tea.KeyMsg) (submit bool, topic string, ttl time.Duration, forever bool) {
	if m.retain == nil {
		return false, "", 0, false
	}
	switch k.Type {
	case tea.KeyEsc:
		m.retain = nil
		return false, "", 0, false
	case tea.KeyEnter:
		var err error
		ttl, forever, err = cli.ParseRetainTTL(m.retain.input.Value())
		if err != nil {
			m.status = "retain: " + err.Error()
			return false, "", 0, false
		}
		topic = m.retain.topic
		m.retain = nil
		return true, topic, ttl, forever
	}
	m.retain.input, _ = m.retain.input.Update(k)
	return false, "", 0, false
}

func (m *BoardModal) retainView() string {
	return m.retain.input.View() + "\n" +
		MutedStyle.Render("enter sets · a duration counts from the last publish, or forever · esc cancels")
}

// retentionOf is topic's retention as the last topic listing reported it,
// nil when it is not retained (or not listed).
func (m *BoardModal) retentionOf(topic string) *cli.TopicRetention {
	for i := range m.rowTopics {
		if m.rowTopics[i].Name == topic {
			return m.rowTopics[i].Retention
		}
	}
	return nil
}

// unretainHint is the footer's "T: unretain", present only when T would do
// something: on a retained topic.
func (m *BoardModal) unretainHint(topic string) string {
	if m.retentionOf(topic) == nil {
		return ""
	}
	return "  " + modalKeys.BoardUnretain + ": unretain"
}

// Unretain reports whether T should stop retaining topic. On a topic that is
// not retained it says so on the status line instead: the key is offered
// only where it acts.
func (m *BoardModal) Unretain(topic string) bool {
	if topic == "" {
		return false
	}
	if m.retentionOf(topic) == nil {
		m.status = "unretain: " + topic + " is not retained"
		return false
	}
	return true
}

// retainCell is the topics table's Retain column: filled on a retained row
// only, with the same keys `board topics` prints.
func retainCell(r *cli.BoardTopicRow) string {
	if r.Retention == nil {
		return ""
	}
	return fmt.Sprintf("%s%s by=%s disk=%d", r.Retention.Label(), r.Retention.UntilLabel(), r.Retention.SetByLabel(), r.Retention.DiskBytes)
}

// boardOpenEdit is the one-line editor `o` opens on the topic list: a topic
// by name, including one nothing was published to yet. Its message view is
// where the TUI starts a topic — `m` sends the first message, `t` retains it.
type boardOpenEdit struct {
	input textinput.Model
}

// BeginOpenTopic opens the name editor.
func (m *BoardModal) BeginOpenTopic() {
	prompt := "open topic ▶ "
	ti := textinput.New()
	ti.Prompt = prompt
	ti.Placeholder = "a topic name, new or existing"
	ti.Width = m.content.Width - len(prompt)
	ti.Focus()
	m.opening = &boardOpenEdit{input: ti}
}

// OpeningTopic reports whether the name editor is open; the App routes every
// key here while it is.
func (m *BoardModal) OpeningTopic() bool { return m.opening != nil }

// HandleOpenTopicKey feeds one key to the name editor. open is true when
// Enter submitted a non-blank name; the editor is closed then.
func (m *BoardModal) HandleOpenTopicKey(k tea.KeyMsg) (open bool, topic string) {
	if m.opening == nil {
		return false, ""
	}
	switch k.Type {
	case tea.KeyEsc:
		m.opening = nil
		return false, ""
	case tea.KeyEnter:
		topic = strings.TrimSpace(m.opening.input.Value())
		if topic == "" {
			return false, ""
		}
		m.opening = nil
		return true, topic
	}
	m.opening.input, _ = m.opening.input.Update(k)
	return false, ""
}

func (m *BoardModal) openTopicView() string {
	return m.opening.input.View() + "\n" +
		MutedStyle.Render("enter opens it, even if nothing was published there yet · then m sends, t retains · esc cancels")
}
