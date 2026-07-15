package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/shlex"
)

// popupFocus tracks which sub-field of the submit popup currently receives
// keystrokes. Cycled by Ctrl+E. The textarea (prompt) is the primary focus;
// the args field is one shlex-parsed line of per-task --claude-arg values;
// the resume field holds an optional 32-hex task id to revive.
type popupFocus int

const (
	popupFocusPrompt popupFocus = iota
	popupFocusArgs
	popupFocusResume
)

type PopupModel struct {
	repoChoices []string
	repoIdx     int
	// hostChoices holds "(any)" followed by the known runner hostnames.
	// hostIdx 0 means no pin (Any selector).
	hostChoices []string
	hostIdx     int
	// agentChoices holds "(default)" followed by the union of agent profiles
	// advertised by currently-known runners. agentIdx 0 means no explicit
	// pick (server resolves the bound runner's default / the resumed task's
	// own profile). Mirrors hostChoices/hostIdx exactly.
	agentChoices []string
	agentIdx     int
	ta           textarea.Model
	args        textinput.Model
	resume      textinput.Model
	resumeConv  bool
	focus       popupFocus
	open        bool
}

func NewPopup(defaultRepo string) PopupModel {
	ta := textarea.New()
	ta.Placeholder = "Type the prompt for claude. Ctrl+J (Ctrl+Enter) to submit, Tab to switch repo, Shift+Tab to cycle host pin, Ctrl+A to cycle agent, Ctrl+E to cycle through args/resume fields, Esc to cancel."
	ta.SetWidth(60)
	ta.SetHeight(10)
	args := textinput.New()
	args.Placeholder = "extra claude args (shell-quoted; e.g. --resume <uuid> --add-dir /repo)"
	args.CharLimit = 4096
	args.Width = 60
	resume := textinput.New()
	resume.Placeholder = "resume task id (32 hex; empty = new task)"
	resume.CharLimit = 64
	resume.Width = 60
	pm := PopupModel{ta: ta, args: args, resume: resume, hostChoices: []string{"(any)"}, agentChoices: []string{"(default)"}}
	if defaultRepo != "" {
		pm.repoChoices = []string{defaultRepo}
	}
	return pm
}

func (m *PopupModel) IsOpen() bool { return m.open }

func (m *PopupModel) Open() {
	m.open = true
	m.hostIdx = 0
	m.agentIdx = 0
	m.ta.Reset()
	m.args.SetValue("")
	m.resume.SetValue("")
	m.resumeConv = false
	m.focus = popupFocusPrompt
	m.ta.Focus()
	m.args.Blur()
	m.resume.Blur()
}

func (m *PopupModel) Close() {
	m.open = false
	m.ta.Blur()
	m.args.Blur()
	m.resume.Blur()
}

// ToggleFocus cycles the active editable field: prompt → args → resume →
// prompt. Used by app.go on Ctrl+E so the user can edit each per-submit
// option without leaving the popup.
func (m *PopupModel) ToggleFocus() {
	switch m.focus {
	case popupFocusPrompt:
		m.focus = popupFocusArgs
		m.ta.Blur()
		m.args.Focus()
		m.resume.Blur()
	case popupFocusArgs:
		m.focus = popupFocusResume
		m.ta.Blur()
		m.args.Blur()
		m.resume.Focus()
	default:
		m.focus = popupFocusPrompt
		m.ta.Focus()
		m.args.Blur()
		m.resume.Blur()
	}
}

// ResumeTaskID returns the trimmed resume task id input, or "" when the user
// did not type one (which the cli layer treats as "new task").
func (m *PopupModel) ResumeTaskID() string {
	v := m.resume.Value()
	// strings.TrimSpace would pull in another import; the input is small so
	// inline trim is fine.
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t') {
		v = v[1:]
	}
	for len(v) > 0 && (v[len(v)-1] == ' ' || v[len(v)-1] == '\t') {
		v = v[:len(v)-1]
	}
	return v
}

func (m *PopupModel) ResumeConversation() bool { return m.resumeConv }

func (m *PopupModel) ToggleResumeConversation() {
	m.resumeConv = !m.resumeConv
}

// ExtraArgs returns the current args input, parsed via shlex so quoted values
// survive (e.g. `--add-dir "C:/Program Files/x"`). On parse error returns the
// best-effort whitespace split so the user still gets something.
func (m *PopupModel) ExtraArgs() []string {
	raw := m.args.Value()
	if raw == "" {
		return nil
	}
	if parsed, err := shlex.Split(raw); err == nil {
		return parsed
	}
	// Fallback: simple whitespace split so a bad quote doesn't drop user input.
	out := []string{}
	cur := ""
	for _, r := range raw {
		if r == ' ' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func (m *PopupModel) Repo() string {
	if len(m.repoChoices) == 0 {
		return ""
	}
	return m.repoChoices[m.repoIdx]
}
func (m *PopupModel) Prompt() string { return m.ta.Value() }

// Host returns the selected pin hostname, or "" when "(any)" is selected.
func (m *PopupModel) Host() string {
	if m.hostIdx == 0 || len(m.hostChoices) == 0 {
		return ""
	}
	return m.hostChoices[m.hostIdx]
}

// SetHostChoices replaces the list of selectable hosts.
// The first entry is always "(any)" (no pin); then the supplied hostnames follow.
// Empty strings in hosts are skipped.
func (m *PopupModel) SetHostChoices(hosts []string) {
	final := []string{"(any)"}
	for _, h := range hosts {
		if h != "" {
			final = append(final, h)
		}
	}
	m.hostChoices = final
	m.hostIdx = 0
}

// CycleHost advances host selection by step (1 = next, -1 = prev), wrapping
// around. No-op when there are 0 or 1 choices.
func (m *PopupModel) CycleHost(step int) {
	n := len(m.hostChoices)
	if n <= 1 {
		return
	}
	m.hostIdx = ((m.hostIdx+step)%n + n) % n
}

// Agent returns the selected agent profile, or "" when "(default)" is
// selected (server resolves the bound runner's default profile).
func (m *PopupModel) Agent() string {
	if m.agentIdx == 0 || len(m.agentChoices) == 0 {
		return ""
	}
	return m.agentChoices[m.agentIdx]
}

// SetAgentChoices replaces the list of selectable agent profiles.
// The first entry is always "(default)" (no explicit pick); then the
// supplied profile names follow. Empty strings in profiles are skipped.
func (m *PopupModel) SetAgentChoices(profiles []string) {
	final := []string{"(default)"}
	for _, p := range profiles {
		if p != "" {
			final = append(final, p)
		}
	}
	m.agentChoices = final
	m.agentIdx = 0
}

// CycleAgent advances agent-profile selection by step (1 = next, -1 = prev),
// wrapping around. No-op when there are 0 or 1 choices.
func (m *PopupModel) CycleAgent(step int) {
	n := len(m.agentChoices)
	if n <= 1 {
		return
	}
	m.agentIdx = ((m.agentIdx+step)%n + n) % n
}

// SetRepo sets a single-choice repo (no cycling). Convenience for callers
// that only have a default and no runner registry context.
func (m *PopupModel) SetRepo(r string) {
	if r == "" {
		m.repoChoices = nil
	} else {
		m.repoChoices = []string{r}
	}
	m.repoIdx = 0
}

// SetRepoChoices replaces the list of selectable repos and starts at the
// entry equal to `current`. If `current` is non-empty and not in `choices`,
// it is prepended so the user's explicit setting is never silently dropped.
// Empty strings in `choices` are skipped.
func (m *PopupModel) SetRepoChoices(choices []string, current string) {
	final := make([]string, 0, len(choices)+1)
	if current != "" {
		final = append(final, current)
	}
	for _, c := range choices {
		if c == "" || c == current {
			continue
		}
		final = append(final, c)
	}
	m.repoChoices = final
	m.repoIdx = 0
}

// CycleRepo advances repo selection by step (1 = next, -1 = prev), wrapping
// around. No-op when there are 0 or 1 choices.
func (m *PopupModel) CycleRepo(step int) {
	n := len(m.repoChoices)
	if n <= 1 {
		return
	}
	m.repoIdx = ((m.repoIdx+step)%n + n) % n
}

func (m PopupModel) Update(msg tea.Msg) (PopupModel, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	switch m.focus {
	case popupFocusArgs:
		m.args, cmd = m.args.Update(msg)
	case popupFocusResume:
		m.resume, cmd = m.resume.Update(msg)
	default:
		m.ta, cmd = m.ta.Update(msg)
	}
	return m, cmd
}

func (m PopupModel) View() string {
	if !m.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	repo := m.Repo()
	if repo == "" {
		repo = "(none — no runners registered)"
	}
	header := "New task — repo: " + repo
	if n := len(m.repoChoices); n > 1 {
		header += fmt.Sprintf("  [Tab: switch repo (%d/%d)]", m.repoIdx+1, n)
	}
	host := m.Host()
	if host == "" {
		host = "(any)"
	}
	header += "\n             host: " + host
	if n := len(m.hostChoices); n > 1 {
		header += fmt.Sprintf("  [Shift+Tab: cycle (%d/%d)]", m.hostIdx+1, n)
	}
	agent := m.Agent()
	if agent == "" {
		agent = "(default)"
	}
	header += "\n            agent: " + agent
	if n := len(m.agentChoices); n > 1 {
		header += fmt.Sprintf("  [Ctrl+A: cycle (%d/%d)]", m.agentIdx+1, n)
	}
	resumeConv := "off"
	if m.resumeConv {
		resumeConv = "on"
	}
	header += "\nresume conversation: " + resumeConv + "  [Ctrl+R: toggle]"
	argsLabel := "args:"
	switch {
	case m.focus == popupFocusArgs:
		argsLabel = "args (editing — Ctrl+E to advance):"
	case m.args.Value() == "":
		argsLabel = "args (Ctrl+E to edit):"
	}
	resumeLabel := "resume:"
	switch {
	case m.focus == popupFocusResume:
		resumeLabel = "resume (editing — Ctrl+E to advance):"
	case m.resume.Value() == "":
		resumeLabel = "resume (Ctrl+E to edit; empty = new task):"
	}
	footer := FooterStyle.Render("Ctrl+J: submit  ·  Tab: next repo  ·  Shift+Tab: cycle host  ·  Ctrl+A: cycle agent  ·  Ctrl+E: cycle args/resume  ·  Ctrl+R: resume conversation  ·  Esc: cancel")
	return box.Render(header + "\n\n" + m.ta.View() +
		"\n\n" + argsLabel + "\n" + m.args.View() +
		"\n\n" + resumeLabel + "\n" + m.resume.View() +
		"\n\n" + footer)
}
