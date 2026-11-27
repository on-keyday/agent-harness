package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
)

// ActionsModal is the TUI's Actions screen (M): the rows of `action ls`, with
// n new, e / enter edit, x delete (y/n), r reload, esc close; New and Edit
// open ActionFormModel inside it. Writes go through cli.RunActionPut and
// RunActionRemove, the funnels the command line uses, and their lines go to
// the results pane. A refused save keeps the form and what was typed.
type ActionsModal struct {
	open    bool
	mode    actionsMode
	views   []cli.ActionView
	loadErr error
	cur     int
	form    ActionFormModel
	status  string
	w, h    int
}

type actionsMode int

const (
	actionsList actionsMode = iota
	actionsForm
	actionsConfirmDelete
)

// ActionViewsMsg carries an action list fetched for a screen. For says which:
// "modal" (the Actions modal), "run" (the run form), "picker" (the authority
// picker's condition checklist).
type ActionViewsMsg struct {
	Views []cli.ActionView
	Err   error
	For   string
}

// ActionFormResultMsg is the answer to a save from the form.
type ActionFormResultMsg struct {
	Text string
	OK   bool
	Err  error
}

func DoActionViews(c *cli.Client, forWhat string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		list, err := cli.ActionListWith(ctx, c)
		return ActionViewsMsg{Views: cli.ActionViews(list), Err: err, For: forWhat}
	}
}

// DoActionPutForm sends the form through the same funnel `action new` /
// `action edit` use.
func DoActionPutForm(c *cli.Client, f cli.ActionForm) tea.Cmd {
	return func() tea.Msg {
		in, err := cli.ActionPutInputFromForm(f)
		if err != nil {
			return ActionFormResultMsg{Err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		text, ok, err := c.RunActionPut(ctx, in)
		return ActionFormResultMsg{Text: text, OK: ok, Err: err}
	}
}

func NewActionsModal() ActionsModal { return ActionsModal{} }

func (m *ActionsModal) IsOpen() bool { return m.open }
func (m *ActionsModal) Open()        { m.open, m.mode, m.status = true, actionsList, "" }
func (m *ActionsModal) Close()       { m.open = false }
func (m *ActionsModal) SetSize(w, h int) {
	m.w, m.h = w, h
}

// SetViews replaces the list (a fetch answered).
func (m *ActionsModal) SetViews(v []cli.ActionView, err error) {
	m.views, m.loadErr = v, err
	if m.cur >= len(m.views) {
		m.cur = len(m.views) - 1
	}
	if m.cur < 0 {
		m.cur = 0
	}
}

func (m *ActionsModal) selected() *cli.ActionView {
	if m.cur < 0 || m.cur >= len(m.views) {
		return nil
	}
	return &m.views[m.cur]
}

// OpenForm opens New, or Edit of the selected action when edit.
func (m *ActionsModal) OpenForm(edit bool) {
	if edit {
		v := m.selected()
		if v == nil {
			return
		}
		m.form = NewActionForm(v)
	} else {
		m.form = NewActionForm(nil)
	}
	m.mode = actionsForm
}

// ApplyResult takes a save's answer: ok goes back to the list (the caller
// re-fetches it); a refusal keeps the form and shows the server's line.
func (m *ActionsModal) ApplyResult(r ActionFormResultMsg) {
	switch {
	case r.Err != nil:
		m.form.err = r.Err.Error()
	case !r.OK:
		m.form.err = strings.TrimSpace(r.Text)
	default:
		m.mode = actionsList
		m.status = strings.TrimSpace(r.Text)
	}
}

// actionLines is one action as `action ls` prints it (cli.FormatActions'
// words), so the screen has no second wording.
func actionLines(v cli.ActionView) []string {
	out := []string{fmt.Sprintf("%s rev=%d by=%s", v.Name, v.Revision, v.EditedBy), "  run: " + v.TemplateText}
	for _, p := range v.Params {
		if p.Type == "choice" {
			q := make([]string, len(p.Choices))
			for i, c := range p.Choices {
				q[i] = fmt.Sprintf("%q", c)
			}
			out = append(out, fmt.Sprintf("  param %s: choice %s", p.Name, strings.Join(q, " ")))
		} else {
			out = append(out, fmt.Sprintf("  param %s: %s", p.Name, p.Type))
		}
	}
	if v.Description != "" {
		out = append(out, "  description: "+v.Description)
	}
	return out
}

func (m *ActionsModal) View() string {
	var b strings.Builder
	switch m.mode {
	case actionsForm:
		title := "action new"
		if m.form.form.Ref != "" {
			title = fmt.Sprintf("action edit %s (rev %d)", m.form.form.Name, m.form.form.Revision)
		}
		b.WriteString(FocusedStyle.Render(title) + "\n\n" + m.form.View())
	default:
		b.WriteString(FocusedStyle.Render("Actions") + "\n\n")
		switch {
		case m.loadErr != nil:
			b.WriteString(ErrorStyle.Render(m.loadErr.Error()) + "\n")
		case len(m.views) == 0:
			b.WriteString("no actions — n defines one\n")
		}
		for i, v := range m.views {
			for j, l := range actionLines(v) {
				mark := "  "
				if i == m.cur && j == 0 {
					mark = "▸ "
				}
				b.WriteString(mark + l + "\n")
			}
		}
		if m.mode == actionsConfirmDelete {
			if v := m.selected(); v != nil {
				b.WriteString(WarnStyle.Render(fmt.Sprintf("delete action %s? a grant naming it then runs nothing (y/n)", v.Name)) + "\n")
			}
		} else if m.status != "" {
			b.WriteString(m.status + "\n")
		}
		b.WriteString(MutedStyle.Render("↑/↓ select · n new · e edit · x delete · r reload · esc close"))
	}
	w := m.w - 4
	if w > 110 {
		w = 110
	}
	if w < 40 {
		w = 40
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(w).Render(b.String())
}
