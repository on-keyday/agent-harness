package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type PopupModel struct {
	repoChoices []string
	repoIdx     int
	ta          textarea.Model
	open        bool
}

func NewPopup(defaultRepo string) PopupModel {
	ta := textarea.New()
	ta.Placeholder = "Type the prompt for claude. Ctrl+J (Ctrl+Enter) to submit, Tab to switch repo, Esc to cancel."
	ta.SetWidth(60)
	ta.SetHeight(10)
	pm := PopupModel{ta: ta}
	if defaultRepo != "" {
		pm.repoChoices = []string{defaultRepo}
	}
	return pm
}

func (m *PopupModel) IsOpen() bool { return m.open }

func (m *PopupModel) Open() {
	m.open = true
	m.ta.Reset()
	m.ta.Focus()
}

func (m *PopupModel) Close() {
	m.open = false
	m.ta.Blur()
}

func (m *PopupModel) Repo() string {
	if len(m.repoChoices) == 0 {
		return ""
	}
	return m.repoChoices[m.repoIdx]
}
func (m *PopupModel) Prompt() string { return m.ta.Value() }

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
	m.ta, cmd = m.ta.Update(msg)
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
		header += fmt.Sprintf("  [Tab/Shift+Tab: switch (%d/%d)]", m.repoIdx+1, n)
	}
	footer := FooterStyle.Render("Ctrl+J: submit  ·  Esc: cancel")
	return box.Render(header + "\n\n" + m.ta.View() + "\n\n" + footer)
}
