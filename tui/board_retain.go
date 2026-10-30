package tui

import (
	"context"
	"fmt"
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

// parseRetainInput reads the retain editor's line: "clear", or what
// `board retain --ttl` takes, through the CLI's own parser.
func parseRetainInput(s string) (ttl time.Duration, forever, clear bool, err error) {
	if s == "clear" {
		return 0, false, true, nil
	}
	ttl, forever, err = cli.ParseRetainTTL(s)
	return ttl, forever, false, err
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
	ti.Placeholder = "72h, forever, or clear"
	ti.Width = m.content.Width - len(prompt)
	ti.Focus()
	m.retain = &boardRetainEdit{input: ti, topic: topic}
}

// Retaining reports whether the retain editor is open; the App routes every
// key here while it is.
func (m *BoardModal) Retaining() bool { return m.retain != nil }

// HandleRetainKey feeds one key to the retain editor. submit is true when
// Enter produced a valid request; the editor is closed then. A value that
// does not parse keeps the editor open with the reason on the status line.
func (m *BoardModal) HandleRetainKey(k tea.KeyMsg) (submit bool, topic string, ttl time.Duration, forever, clear bool) {
	if m.retain == nil {
		return false, "", 0, false, false
	}
	switch k.Type {
	case tea.KeyEsc:
		m.retain = nil
		return false, "", 0, false, false
	case tea.KeyEnter:
		var err error
		ttl, forever, clear, err = parseRetainInput(m.retain.input.Value())
		if err != nil {
			m.status = "retain: " + err.Error()
			return false, "", 0, false, false
		}
		topic = m.retain.topic
		m.retain = nil
		return true, topic, ttl, forever, clear
	}
	m.retain.input, _ = m.retain.input.Update(k)
	return false, "", 0, false, false
}

func (m *BoardModal) retainView() string {
	return m.retain.input.View() + "\n" +
		MutedStyle.Render("enter sets · a duration counts from the last publish · forever · clear returns to the default TTL · esc cancels")
}

// retainCell is the topics table's Retain column: filled on a retained row
// only, with the same keys `board topics` prints.
func retainCell(r *cli.BoardTopicRow) string {
	if r.Retention == nil {
		return ""
	}
	return fmt.Sprintf("%s by=%s disk=%d", r.Retention.Label(), r.Retention.SetByLabel(), r.Retention.DiskBytes)
}
