package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// bgFormPurpose is what the background form submits (pillar A spec, A-D15).
type bgFormPurpose int

const (
	bgOpenNew bgFormPurpose = iota // a new background session
	bgResume                       // a background resume of an ended task
	bgSwitch                       // a running session switched to background
)

// The form's fields after the prompt, in Tab order.
const (
	bgFieldAgent = iota
	bgFieldDeadline
	bgFieldTurnEnd
	bgFieldQuiet
	bgFieldBlocked
	bgFieldQueue
	bgFieldCount
)

var bgFieldLabels = [bgFieldCount]string{"agent", "deadline", "turn end", "quiet", "blocked", "queue timeout"}

// defaultBgDeadline is the deadline the form starts with: the front-end
// prefill G41 allows (the server has no default).
const defaultBgDeadline = "1h"

// BgFormModel collects a background open, a background resume, or a switch to
// background. The prompt and the deadline are what a background run cannot do
// without; a resume is prefilled from the task's last run (A-D7), since the
// server reuses nothing.
type BgFormModel struct {
	open               bool
	purpose            bgFormPurpose
	repo               string
	resumeID           string // bgResume: the task to resume
	resumeConversation bool
	switchID           string // bgSwitch: the running task to switch
	stream             bool
	prompt             textarea.Model
	fields             [bgFieldCount]textinput.Model
	focus              int // -1 = prompt; otherwise a field index
	// pin is the runner a resume is pinned to (r/R: where the task last ran,
	// while it is connected); zero leaves it to placement.
	pin protocol.RunnerID
	// pending is set while a submitted request is unanswered: the form stays
	// open until the server answers, so a refusal keeps what was typed.
	pending bool
	errMsg  string
}

func NewBgForm() BgFormModel {
	ta := textarea.New()
	ta.Placeholder = "the agent's first turn (at most 16384 bytes)"
	ta.SetWidth(64)
	ta.SetHeight(6)
	// No limits in the widget: a rune limit silently cut a paste down (and
	// counts runes, not the bytes the cap is in), and MaxHeight silently
	// refused a newline past 99 lines. Values checks the cap and says so.
	ta.CharLimit = 0
	ta.MaxHeight = 0
	m := BgFormModel{prompt: ta}
	for i := range m.fields {
		in := textinput.New()
		in.Width = 30
		in.CharLimit = 64
		m.fields[i] = in
	}
	m.fields[bgFieldTurnEnd].Placeholder = "any (off|quiet|lifecycle|program|any)"
	m.fields[bgFieldQuiet].Placeholder = "5m"
	m.fields[bgFieldBlocked].Placeholder = "5m"
	m.fields[bgFieldQueue].Placeholder = "none"
	return m
}

func (m *BgFormModel) IsOpen() bool     { return m.open }
func (m *BgFormModel) IsSwitch() bool   { return m.purpose == bgSwitch }
func (m *BgFormModel) ResumeID() string { return m.resumeID }
func (m *BgFormModel) SwitchID() string { return m.switchID }
func (m *BgFormModel) Repo() string     { return m.repo }
func (m *BgFormModel) Stream() bool     { return m.stream }
func (m *BgFormModel) ResumeConversation() bool {
	return m.resumeConversation
}

func (m *BgFormModel) reset(purpose bgFormPurpose) {
	m.open, m.purpose = true, purpose
	m.repo, m.resumeID, m.switchID, m.stream, m.resumeConversation = "", "", "", false, false
	m.pin, m.pending, m.errMsg = protocol.RunnerID{}, false, ""
	m.prompt.Reset()
	for i := range m.fields {
		m.fields[i].SetValue("")
	}
	m.fields[bgFieldDeadline].SetValue(defaultBgDeadline)
	m.setFocus(-1)
	if purpose == bgSwitch {
		m.setFocus(bgFieldDeadline)
	}
}

// OpenNew starts a new background session on repo.
func (m *BgFormModel) OpenNew(repo, agent string) {
	m.reset(bgOpenNew)
	m.repo = repo
	m.fields[bgFieldAgent].SetValue(agent)
}

// OpenResume starts a background resume of t, prefilled from its last run:
// prompt, agent and conditions (the deadline from the run, 1h when it had
// none). pin is the runner to resume on, zero for any.
func (m *BgFormModel) OpenResume(t protocol.TaskInfo, idHex string, resumeConversation bool, pin protocol.RunnerID) {
	m.reset(bgResume)
	m.resumeID = idHex
	m.pin = pin
	m.resumeConversation = resumeConversation
	m.stream = t.Kind == protocol.TaskKind_Stream
	m.prompt.SetValue(string(t.Prompt))
	m.fields[bgFieldAgent].SetValue(string(t.AgentProfile))
	c := t.Conditions
	if c.DeadlineMs != 0 {
		m.fields[bgFieldDeadline].SetValue(msDuration(c.DeadlineMs))
	}
	if c.TurnEnd != protocol.TurnEndSignal_Unset {
		m.fields[bgFieldTurnEnd].SetValue(cli.EnumWireName(c.TurnEnd.String()))
	}
	if c.QuietMs != 0 {
		m.fields[bgFieldQuiet].SetValue(msDuration(c.QuietMs))
	}
	if c.BlockedMs != 0 {
		m.fields[bgFieldBlocked].SetValue(msDuration(c.BlockedMs))
	}
	if c.QueueTimeoutMs != 0 {
		m.fields[bgFieldQueue].SetValue(msDuration(c.QueueTimeoutMs))
	}
}

// OpenSwitch asks for the conditions of a running session going background;
// it has no prompt (the run is under way) and no agent.
func (m *BgFormModel) OpenSwitch(idHex string) {
	m.reset(bgSwitch)
	m.switchID = idHex
}

func (m *BgFormModel) Close() {
	m.open = false
	m.prompt.Blur()
	for i := range m.fields {
		m.fields[i].Blur()
	}
}

func msDuration(ms uint64) string {
	return (time.Duration(ms) * time.Millisecond).String()
}

// order is the Tab cycle: prompt then fields, the prompt and agent skipped
// on a switch.
func (m *BgFormModel) order() []int {
	if m.purpose == bgSwitch {
		return []int{bgFieldDeadline, bgFieldTurnEnd, bgFieldQuiet, bgFieldBlocked}
	}
	return []int{-1, bgFieldAgent, bgFieldDeadline, bgFieldTurnEnd, bgFieldQuiet, bgFieldBlocked, bgFieldQueue}
}

func (m *BgFormModel) setFocus(f int) {
	m.focus = f
	if f == -1 {
		m.prompt.Focus()
	} else {
		m.prompt.Blur()
	}
	for i := range m.fields {
		if i == f {
			m.fields[i].Focus()
		} else {
			m.fields[i].Blur()
		}
	}
}

// Cycle moves the focus by d through the Tab order.
func (m *BgFormModel) Cycle(d int) {
	ord := m.order()
	at := 0
	for i, f := range ord {
		if f == m.focus {
			at = i
		}
	}
	m.setFocus(ord[(at+d+len(ord))%len(ord)])
}

// Update feeds a key to the focused field.
func (m *BgFormModel) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.focus == -1 {
		m.prompt, cmd = m.prompt.Update(msg)
		return cmd
	}
	m.fields[m.focus], cmd = m.fields[m.focus].Update(msg)
	return cmd
}

func parseFormDuration(name, v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s: %q is not a duration (e.g. 90s, 5m, 1h)", name, v)
	}
	return d, nil
}

// Values reads the form: the run-mode flags (Background set unless it is a
// switch) and the agent.
func (m *BgFormModel) Values() (cli.RunModeFlags, string, error) {
	var f cli.RunModeFlags
	var err error
	if f.Deadline, err = parseFormDuration("deadline", m.fields[bgFieldDeadline].Value()); err != nil {
		return f, "", err
	}
	if f.Quiet, err = parseFormDuration("quiet", m.fields[bgFieldQuiet].Value()); err != nil {
		return f, "", err
	}
	if f.Blocked, err = parseFormDuration("blocked", m.fields[bgFieldBlocked].Value()); err != nil {
		return f, "", err
	}
	if f.QueueTimeout, err = parseFormDuration("queue timeout", m.fields[bgFieldQueue].Value()); err != nil {
		return f, "", err
	}
	if f.TurnEnd, err = verb.ParseTurnEnd(strings.TrimSpace(m.fields[bgFieldTurnEnd].Value())); err != nil {
		return f, "", err
	}
	agent := strings.TrimSpace(m.fields[bgFieldAgent].Value())
	if m.purpose != bgSwitch {
		f.Background = true
		f.Prompt = m.prompt.Value()
		switch {
		case strings.TrimSpace(f.Prompt) == "":
			return f, "", fmt.Errorf("prompt: a background run needs one")
		case len(f.Prompt) > protocol.PromptMax:
			return f, "", fmt.Errorf("prompt: %d bytes, over the %d-byte cap", len(f.Prompt), protocol.PromptMax)
		}
	}
	if m.purpose == bgOpenNew && agent == "" {
		return f, "", fmt.Errorf("agent: a new background session names one")
	}
	return f, agent, nil
}

// Submitted marks the form's request as sent; Answered takes the server's
// answer: success closes the form, a refusal keeps it open with the reason.
func (m *BgFormModel) Submitted()    { m.pending, m.errMsg = true, "" }
func (m *BgFormModel) Pending() bool { return m.open && m.pending }
func (m *BgFormModel) Answered(err error) {
	m.pending = false
	if err == nil {
		m.Close()
		return
	}
	m.errMsg = err.Error()
}

func (m *BgFormModel) View() string {
	var sb strings.Builder
	switch m.purpose {
	case bgOpenNew:
		fmt.Fprintf(&sb, "New background session on %s\n\n", m.repo)
	case bgResume:
		fmt.Fprintf(&sb, "Resume %s in background\n\n", shortTaskID(m.resumeID))
	case bgSwitch:
		fmt.Fprintf(&sb, "Switch %s to background\n\n", shortTaskID(m.switchID))
	}
	if m.purpose != bgSwitch {
		sb.WriteString("prompt:\n" + m.prompt.View() + "\n")
	}
	for _, i := range m.order() {
		if i < 0 {
			continue
		}
		fmt.Fprintf(&sb, "%-14s %s\n", bgFieldLabels[i]+":", m.fields[i].View())
	}
	switch {
	case m.pending:
		sb.WriteString("\nsending…")
	case m.errMsg != "":
		sb.WriteString("\n" + ErrorStyle.Render(m.errMsg))
	}
	sb.WriteString("\nTab/Shift+Tab move · Ctrl+J start · Esc cancel")
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Render(sb.String())
}

func (m *BgFormModel) Pin() protocol.RunnerID { return m.pin }
