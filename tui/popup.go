package tui

import (
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type PopupModel struct {
	repo string
	ta   textarea.Model
	open bool
}

func NewPopup(defaultRepo string) PopupModel {
	ta := textarea.New()
	ta.Placeholder = "Type the prompt for claude. Ctrl+J (Ctrl+Enter) to submit, Esc to cancel."
	ta.SetWidth(60)
	ta.SetHeight(10)
	return PopupModel{repo: defaultRepo, ta: ta}
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

func (m *PopupModel) Repo() string   { return m.repo }
func (m *PopupModel) Prompt() string { return m.ta.Value() }

func (m *PopupModel) SetRepo(r string) { m.repo = r }

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
	header := "New task — repo: " + m.repo
	footer := FooterStyle.Render("Ctrl+J: submit  ·  Esc: cancel")
	return box.Render(header + "\n\n" + m.ta.View() + "\n\n" + footer)
}
