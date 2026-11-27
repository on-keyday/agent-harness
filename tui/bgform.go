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
	bgOpenNew  bgFormPurpose = iota // a new background session
	bgResume                        // a background resume of an ended task
	bgSwitch                        // a running session switched to background
	bgSchedule                      // a new schedule (schedule new)
)

// The form's fields after the prompt, in Tab order.
const (
	bgFieldAgent = iota
	bgFieldDeadline
	bgFieldTurnEnd
	bgFieldQuiet
	bgFieldBlocked
	bgFieldQueue
	bgFieldRecord
	bgFieldPty
	// The schedule form's own (bgSchedule only).
	bgFieldCron
	bgFieldZone
	bgFieldRepo
	bgFieldTarget
	bgFieldKind // interactive | oneshot | stream: one kind, as the WebUI's radio
	bgFieldConv
	bgFieldCount
)

// bgFocusAuthority is the schedule form's authority row: not a text field,
// Enter on it opens the authority picker (PickerModeSchedule).
const bgFocusAuthority = -2

var bgFieldLabels = [bgFieldCount]string{"agent", "deadline", "turn end", "quiet", "blocked", "queue timeout", "record", "pty",
	"cron", "zone", "repo", "task", "kind", "conversation"}

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
	// editID is the schedule this form edits ("" = a new schedule); editBase
	// its stored values, which carry what the form has no field for (agent
	// args, runner); editWas the form as opened, read back.
	editID   string
	editBase cli.ScheduleForm
	editWas  cli.ScheduleForm
	// auth is a schedule's authority (the authority row): a new one's starts
	// as the session defaults, an edit's as stored; the picker replaces it.
	auth Authority
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
	// The PTY record (A-D22): left as default the open says nothing and the
	// server's --record decides; a resume is not prefilled from the last run,
	// which the server would not reuse either.
	// --pty / --no-pty (A-D33): left as default the open says nothing and the
	// server takes the mode's default (a PTY here: the form opens no one-shot).
	// Both placeholders are set in reset: an edit narrows them.
	m.fields[bgFieldCron].Placeholder = "0 9 * * *  or  @daily"
	m.fields[bgFieldZone].Placeholder = "the server's zone (or e.g. Asia/Tokyo)"
	m.fields[bgFieldTarget].Placeholder = "new task (or a task id / its prefix to bind)"
	m.fields[bgFieldKind].Placeholder = "interactive|oneshot|stream"
	m.fields[bgFieldConv].Placeholder = "no (no|yes): each firing resumes the conversation"
	m.fields[bgFieldRepo].CharLimit = 4096
	return m
}

func (m *BgFormModel) IsOpen() bool     { return m.open }
func (m *BgFormModel) IsSwitch() bool   { return m.purpose == bgSwitch }
func (m *BgFormModel) IsSchedule() bool { return m.purpose == bgSchedule }

// IsScheduleEdit: the schedule form edits an existing schedule.
func (m *BgFormModel) IsScheduleEdit() bool { return m.purpose == bgSchedule && m.editID != "" }
func (m *BgFormModel) ResumeID() string     { return m.resumeID }
func (m *BgFormModel) SwitchID() string     { return m.switchID }
func (m *BgFormModel) Repo() string         { return m.repo }
func (m *BgFormModel) Stream() bool         { return m.stream }
func (m *BgFormModel) ResumeConversation() bool {
	return m.resumeConversation
}

func (m *BgFormModel) reset(purpose bgFormPurpose) {
	m.open, m.purpose = true, purpose
	m.repo, m.resumeID, m.switchID, m.stream, m.resumeConversation = "", "", "", false, false
	m.pin, m.pending, m.errMsg = protocol.RunnerID{}, false, ""
	m.editID, m.editBase, m.editWas, m.auth = "", cli.ScheduleForm{}, cli.ScheduleForm{}, Authority{}
	m.prompt.Reset()
	for i := range m.fields {
		m.fields[i].SetValue("")
	}
	m.fields[bgFieldDeadline].SetValue(defaultBgDeadline)
	m.fields[bgFieldRecord].Placeholder = "default (default|on|off)"
	m.fields[bgFieldPty].Placeholder = "default (default|on|off)"
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

// OpenSchedule starts a new schedule: its first firing creates a task on
// repo, or the task field names one to bind.
func (m *BgFormModel) OpenSchedule(repo string, auth Authority) {
	m.reset(bgSchedule)
	m.auth = auth
	m.fields[bgFieldRepo].SetValue(repo)
	m.setFocus(bgFieldCron)
}

// OpenScheduleEdit opens the schedule form on an existing schedule, filled
// with its values (cli.ScheduleFormOf). Saving sends only what changed
// (ScheduleEditArgs); the repo and the bound task are not editable, and what
// the form has no field for is kept as stored.
func (m *BgFormModel) OpenScheduleEdit(id string, f cli.ScheduleForm, auth Authority) {
	m.reset(bgSchedule)
	m.editID, m.editBase, m.auth = id, f, auth
	yesNo := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	orDefault := func(s string) string {
		if s == "" {
			return "default"
		}
		return s
	}
	for i, v := range map[int]string{
		bgFieldCron: f.Cron, bgFieldZone: f.Tz, bgFieldRepo: f.Repo, bgFieldTarget: f.Task, bgFieldAgent: f.Agent,
		bgFieldDeadline: f.Deadline, bgFieldTurnEnd: f.TurnEnd, bgFieldQuiet: f.Quiet, bgFieldBlocked: f.Blocked,
		bgFieldQueue: f.QueueTimeout, bgFieldRecord: orDefault(f.Record), bgFieldPty: orDefault(f.Pty),
		bgFieldKind: f.Kind,
		bgFieldConv: yesNo(f.ResumeConversation),
	} {
		m.fields[i].SetValue(v)
	}
	// A stored schedule's record and pty are on or off: create resolved the
	// default (server/schedule_handler.go), so an edit has none to offer.
	m.fields[bgFieldRecord].Placeholder = "on|off"
	m.fields[bgFieldPty].Placeholder = "on|off"
	m.prompt.SetValue(f.Prompt)
	m.editWas, _ = m.scheduleFormValue()
	m.setFocus(bgFieldCron)
}

// scheduleFormValue is the form's fields over editBase: the schedule as it
// would be saved.
func (m *BgFormModel) scheduleFormValue() (cli.ScheduleForm, error) {
	val := func(i int) string { return strings.TrimSpace(m.fields[i].Value()) }
	f := m.editBase
	au := m.auth
	f.SetAuthority(au.Caps, au.Scope, au.Overrides, au.GrantConditions, au.DelegateCaps, au.DelegateScope, au.SpawnDepth, au.MaxDescendants)
	f.Cron, f.Tz, f.Agent, f.Prompt = val(bgFieldCron), val(bgFieldZone), val(bgFieldAgent), m.prompt.Value()
	f.Deadline, f.TurnEnd, f.Quiet, f.Blocked, f.QueueTimeout =
		val(bgFieldDeadline), val(bgFieldTurnEnd), val(bgFieldQuiet), val(bgFieldBlocked), val(bgFieldQueue)
	for _, t := range []struct {
		i   int
		out *string
	}{{bgFieldRecord, &f.Record}, {bgFieldPty, &f.Pty}} {
		switch v := val(t.i); v {
		case "", "default":
			*t.out = ""
		case "on", "off":
			*t.out = v
		default:
			return f, fmt.Errorf("%s: %q: want default, on or off", bgFieldLabels[t.i], v)
		}
	}
	kind, err := m.scheduleKind()
	if err != nil {
		return f, err
	}
	f.Kind = kind
	switch v := val(bgFieldConv); v {
	case "", "no":
		f.ResumeConversation = false
	case "yes":
		f.ResumeConversation = true
	default:
		return f, fmt.Errorf("%s: %q: want no or yes", bgFieldLabels[bgFieldConv], v)
	}
	return f, nil
}

// scheduleKind is the kind field: one of interactive, oneshot, stream
// ("" = interactive).
func (m *BgFormModel) scheduleKind() (string, error) {
	switch v := strings.TrimSpace(m.fields[bgFieldKind].Value()); v {
	case "", "interactive":
		return "interactive", nil
	case "oneshot", "stream":
		return v, nil
	default:
		return "", fmt.Errorf("kind: %q: want interactive, oneshot or stream", v)
	}
}

// Authority is the schedule form's authority; SetAuthority replaces it (the
// picker's apply).
func (m *BgFormModel) Authority() Authority      { return m.auth }
func (m *BgFormModel) SetAuthority(au Authority) { m.auth = au }
func (m *BgFormModel) AuthorityFocused() bool    { return m.open && m.focus == bgFocusAuthority }

// Hide and Show step the form aside for the authority picker and back,
// keeping what was typed (Close and the Open* calls reset it).
func (m *BgFormModel) Hide() { m.open = false }
func (m *BgFormModel) Show() { m.open = true }

// ScheduleEditArgs is `schedule edit`'s line for what the operator changed
// (cli.ScheduleEditArgs); just the id when nothing did.
func (m *BgFormModel) ScheduleEditArgs() ([]string, error) {
	now, err := m.scheduleFormValue()
	if err != nil {
		return nil, err
	}
	return cli.ScheduleEditArgs(m.editID, m.editWas, now)
}

// EditID is the schedule being edited.
func (m *BgFormModel) EditID() string { return m.editID }

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
	if m.purpose == bgSchedule && m.editID != "" {
		// An edit cannot move a schedule to another repo or task.
		return []int{bgFieldCron, bgFieldZone, -1, bgFieldAgent, bgFieldDeadline, bgFieldTurnEnd,
			bgFieldQuiet, bgFieldBlocked, bgFieldQueue, bgFieldRecord, bgFieldPty, bgFieldKind, bgFieldConv, bgFocusAuthority}
	}
	if m.purpose == bgSchedule {
		return []int{bgFieldCron, bgFieldZone, bgFieldRepo, bgFieldTarget, -1, bgFieldAgent, bgFieldDeadline, bgFieldTurnEnd,
			bgFieldQuiet, bgFieldBlocked, bgFieldQueue, bgFieldRecord, bgFieldPty, bgFieldKind, bgFieldConv, bgFocusAuthority}
	}
	return []int{-1, bgFieldAgent, bgFieldDeadline, bgFieldTurnEnd, bgFieldQuiet, bgFieldBlocked, bgFieldQueue, bgFieldRecord, bgFieldPty}
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
	if m.focus < 0 {
		return nil // the authority row takes Enter only (inBgForm)
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
	switch v := strings.TrimSpace(m.fields[bgFieldRecord].Value()); v {
	case "", "default":
	case "on", "off":
		f.RecordSet, f.Record = true, v == "on"
	default:
		return f, "", fmt.Errorf("record: %q: want default, on or off", v)
	}
	switch v := strings.TrimSpace(m.fields[bgFieldPty].Value()); v {
	case "", "default":
	case "on", "off":
		f.PtySet, f.Pty = true, v == "on"
	default:
		return f, "", fmt.Errorf("pty: %q: want default, on or off", v)
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

// ScheduleArgs is the schedule form as the argument line of `schedule new`,
// so the shared declaration parses it and builds the request
// (cli.ScheduleSpecOptsFromAction): the form adds no second builder. resolve
// turns the task field (an id or a prefix) into a full id.
func (m *BgFormModel) ScheduleArgs(resolve func(string) (string, error)) ([]string, error) {
	val := func(i int) string { return strings.TrimSpace(m.fields[i].Value()) }
	cron := val(bgFieldCron)
	if cron == "" {
		return nil, fmt.Errorf("cron: a schedule needs one (e.g. @daily, 0 9 * * *)")
	}
	prompt := m.prompt.Value()
	switch {
	case strings.TrimSpace(prompt) == "":
		return nil, fmt.Errorf("prompt: a schedule's firings need one")
	case len(prompt) > protocol.PromptMax:
		return nil, fmt.Errorf("prompt: %d bytes, over the %d-byte cap", len(prompt), protocol.PromptMax)
	}
	args := []string{"--cron", cron}
	if z := val(bgFieldZone); z != "" {
		args = append(args, "--tz", z)
	}
	agent := val(bgFieldAgent)
	if t := val(bgFieldTarget); t != "" {
		id, err := resolve(t)
		if err != nil {
			return nil, fmt.Errorf("task: %w", err)
		}
		args = append(args, "--task", id)
	} else {
		if val(bgFieldRepo) == "" {
			return nil, fmt.Errorf("repo: a new task needs one")
		}
		if agent == "" {
			return nil, fmt.Errorf("agent: a new task names one")
		}
		args = append(args, "--repo", val(bgFieldRepo))
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	args = append(args, "--prompt", prompt)
	for _, f := range []struct {
		i    int
		flag string
	}{{bgFieldDeadline, "--deadline"}, {bgFieldTurnEnd, "--turn-end"}, {bgFieldQuiet, "--quiet"}, {bgFieldBlocked, "--blocked"}, {bgFieldQueue, "--queue-timeout"}} {
		if v := val(f.i); v != "" {
			args = append(args, f.flag, v)
		}
	}
	for _, f := range []struct {
		i       int
		on, off string
	}{{bgFieldRecord, "--record", "--no-record"}, {bgFieldPty, "--pty", "--no-pty"}} {
		switch v := val(f.i); v {
		case "", "default":
		case "on":
			args = append(args, f.on)
		case "off":
			args = append(args, f.off)
		default:
			return nil, fmt.Errorf("%s: %q: want default, on or off", bgFieldLabels[f.i], v)
		}
	}
	kind, err := m.scheduleKind()
	if err != nil {
		return nil, err
	}
	if kind != "interactive" {
		args = append(args, "--"+kind)
	}
	switch v := val(bgFieldConv); v {
	case "", "no":
	case "yes":
		args = append(args, "--resume-conversation")
	default:
		return nil, fmt.Errorf("%s: %q: want no or yes", bgFieldLabels[bgFieldConv], v)
	}
	// The authority row, in the spellings `schedule new` takes, as the
	// WebUI's Compose sends its authority section.
	var af cli.ScheduleForm
	au := m.auth
	af.SetAuthority(au.Caps, au.Scope, au.Overrides, au.GrantConditions, au.DelegateCaps, au.DelegateScope, au.SpawnDepth, au.MaxDescendants)
	args = append(args, "--caps", af.Caps, "--scope", af.Scope)
	for _, v := range af.ScopeFor {
		args = append(args, "--scope-for", v)
	}
	for _, v := range af.GrantConditions {
		args = append(args, "--grant-condition", v)
	}
	args = append(args, "--delegate-caps", af.DelegateCaps, "--delegate-scope", af.DelegateScope,
		"--spawn-depth", af.SpawnDepth, "--max-descendants", af.MaxDescendants)
	return args, nil
}

// authorityLine is the authority row's text.
func (m *BgFormModel) authorityLine() string {
	var af cli.ScheduleForm
	au := m.auth
	af.SetAuthority(au.Caps, au.Scope, au.Overrides, au.GrantConditions, au.DelegateCaps, au.DelegateScope, au.SpawnDepth, au.MaxDescendants)
	line := "caps=" + af.Caps + " scope=" + af.Scope
	for _, v := range af.ScopeFor {
		line += " +" + v
	}
	line += " delegate=" + af.DelegateCaps + " delegate-scope=" + af.DelegateScope +
		" spawn-depth=" + af.SpawnDepth + " max-descendants=" + af.MaxDescendants
	if m.focus == bgFocusAuthority {
		line = "> " + line + "   (Enter: edit)"
	}
	return line
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
	case bgSchedule:
		if m.editID != "" {
			fmt.Fprintf(&sb, "edit schedule %s — repo %s, task %s (an edit keeps both); only what you change is sent\n\n",
				shortTaskID(m.editID), m.editBase.Repo, orNone(m.editBase.Task))
		} else {
			sb.WriteString("new schedule — each firing resumes its task in background\n\n")
		}
	}
	if m.purpose != bgSwitch && m.purpose != bgSchedule {
		sb.WriteString("prompt:\n" + m.prompt.View() + "\n")
	}
	for _, i := range m.order() {
		if i == bgFocusAuthority {
			fmt.Fprintf(&sb, "%-14s %s\n", "authority:", m.authorityLine())
			continue
		}
		if i < 0 {
			if m.purpose == bgSchedule {
				sb.WriteString("prompt:\n" + m.prompt.View() + "\n")
			}
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
	if m.purpose == bgSchedule && m.editID != "" {
		sb.WriteString("\nTab/Shift+Tab move · Ctrl+J save · Esc cancel")
	} else if m.purpose == bgSchedule {
		sb.WriteString("\nTab/Shift+Tab move · Ctrl+J create schedule · Esc cancel")
	} else {
		sb.WriteString("\nTab/Shift+Tab move · Ctrl+J start · Esc cancel")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Render(sb.String())
}

func (m *BgFormModel) Pin() protocol.RunnerID { return m.pin }

func orNone(s string) string {
	if s == "" {
		return "(created at the first firing)"
	}
	return shortTaskID(s)
}
