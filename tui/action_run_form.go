package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/cli"
)

// ActionRunFormModel runs a server-defined action against one task (the tasks
// table's !): ←/→ picks the action on the first row, then one row per param —
// ←/→ cycles a choice, a token is typed — and enter on the last row runs it.
// A token is checked with Go's rule before anything is sent, and the run is
// the exec path `exec --action` takes.
type ActionRunFormModel struct {
	open    bool
	taskID  string
	views   []cli.ActionView
	loadErr error
	loaded  bool
	ai      int
	choice  map[string]int
	tokens  map[string]string
	cur     int
	input   textinput.Model
	err     string
	w, h    int
}

func NewActionRunForm() ActionRunFormModel {
	in := textinput.New()
	in.Width = 40
	in.CharLimit = 128
	return ActionRunFormModel{input: in}
}

func (m *ActionRunFormModel) IsOpen() bool { return m.open }
func (m *ActionRunFormModel) Close()       { m.open = false }
func (m *ActionRunFormModel) SetSize(w, h int) {
	m.w, m.h = w, h
}

// OpenFor opens the form for a task; the list arrives through SetViews.
func (m *ActionRunFormModel) OpenFor(taskID string) {
	*m = ActionRunFormModel{open: true, taskID: taskID, input: m.input, w: m.w, h: m.h}
	m.input.SetValue("")
	m.input.Blur()
}

func (m *ActionRunFormModel) SetViews(v []cli.ActionView, err error) {
	m.views, m.loadErr, m.loaded = v, err, true
	m.ai, m.cur = 0, 0
	m.resetValues()
}

func (m *ActionRunFormModel) resetValues() {
	m.choice, m.tokens = map[string]int{}, map[string]string{}
	m.input.SetValue("")
}

func (m *ActionRunFormModel) action() *cli.ActionView {
	if m.ai < 0 || m.ai >= len(m.views) {
		return nil
	}
	return &m.views[m.ai]
}

// row 0 is the action; 1..len(params) the params; the last row runs.
func (m *ActionRunFormModel) param() *cli.ActionParamView {
	a := m.action()
	if a == nil || m.cur < 1 || m.cur > len(a.Params) {
		return nil
	}
	return &a.Params[m.cur-1]
}

func (m *ActionRunFormModel) rowCount() int {
	if a := m.action(); a != nil {
		return len(a.Params) + 2
	}
	return 1
}

func (m *ActionRunFormModel) storeToken() {
	if p := m.param(); p != nil && p.Type != "choice" {
		m.tokens[p.Name] = m.input.Value()
	}
}

func (m *ActionRunFormModel) loadToken() {
	if p := m.param(); p != nil && p.Type != "choice" {
		m.input.SetValue(m.tokens[p.Name])
		m.input.CursorEnd()
		m.input.Focus()
		return
	}
	m.input.Blur()
}

// Update applies one key; true means "run it" with a Request that passed.
func (m *ActionRunFormModel) Update(msg tea.KeyMsg) bool {
	if m.action() == nil {
		return false
	}
	switch msg.String() {
	case "up", "shift+tab":
		m.storeToken()
		if m.cur > 0 {
			m.cur--
		}
		m.loadToken()
		return false
	case "down", "tab":
		m.storeToken()
		if m.cur < m.rowCount()-1 {
			m.cur++
		}
		m.loadToken()
		return false
	}
	m.err = ""
	switch {
	case m.cur == 0:
		if n := len(m.views); n > 0 && (msg.String() == "left" || msg.String() == "right") {
			if msg.String() == "right" {
				m.ai = (m.ai + 1) % n
			} else {
				m.ai = (m.ai + n - 1) % n
			}
			m.resetValues()
		}
	case m.cur == m.rowCount()-1:
		if msg.Type == tea.KeyEnter {
			if _, _, err := m.Request(); err != nil {
				m.err = err.Error()
				return false
			}
			return true
		}
	default:
		p := m.param()
		if p.Type == "choice" {
			if n := len(p.Choices); n > 0 && (msg.String() == "left" || msg.String() == "right") {
				at := m.choice[p.Name]
				if msg.String() == "right" {
					at = (at + 1) % n
				} else {
					at = (at + n - 1) % n
				}
				m.choice[p.Name] = at
			}
		} else {
			m.input, _ = m.input.Update(msg)
			m.storeToken()
		}
	}
	return false
}

// Request is the action and its values, refused — naming the param — when a
// token is missing or Go's token rule refuses it.
func (m *ActionRunFormModel) Request() (string, map[string]string, error) {
	m.storeToken()
	a := m.action()
	if a == nil {
		return "", nil, fmt.Errorf("no action to run")
	}
	values := map[string]string{}
	for _, p := range a.Params {
		if p.Type == "choice" {
			if len(p.Choices) == 0 {
				return "", nil, fmt.Errorf("param %s has no choices", p.Name)
			}
			values[p.Name] = p.Choices[m.choice[p.Name]]
			continue
		}
		v := m.tokens[p.Name]
		if !actiondef.ValidToken([]byte(v)) {
			return "", nil, fmt.Errorf("param %s: %q is not a token (1-128 bytes of [0-9A-Za-z._-], not starting with -)", p.Name, v)
		}
		values[p.Name] = v
	}
	return a.Name, values, nil
}

func (m *ActionRunFormModel) View() string {
	var b strings.Builder
	b.WriteString(FocusedStyle.Render("action を実行 — "+pfShortID(m.taskID)) + "\n\n")
	switch {
	case !m.loaded:
		b.WriteString("読み込み中…\n")
	case m.loadErr != nil:
		b.WriteString(ErrorStyle.Render(m.loadErr.Error()) + "\n")
	case len(m.views) == 0:
		b.WriteString("no actions — define one in Actions (M)\n")
	default:
		a := m.action()
		mark := func(i int) string {
			if i == m.cur {
				return "▸ "
			}
			return "  "
		}
		b.WriteString(mark(0) + "action  ◂ " + a.Name + " ▸\n")
		b.WriteString(MutedStyle.Render("          run: "+a.TemplateText) + "\n")
		for i, p := range a.Params {
			if p.Type == "choice" {
				v := ""
				if len(p.Choices) > 0 {
					v = p.Choices[m.choice[p.Name]]
				}
				b.WriteString(fmt.Sprintf("%s%-7s ◂ %s ▸\n", mark(i+1), p.Name, v))
			} else if i+1 == m.cur {
				b.WriteString(fmt.Sprintf("%s%-7s %s\n", mark(i+1), p.Name, m.input.View()))
			} else {
				b.WriteString(fmt.Sprintf("%s%-7s %s\n", mark(i+1), p.Name, m.tokens[p.Name]))
			}
		}
		b.WriteString(mark(len(a.Params)+1) + "[ run ]\n")
		if m.err != "" {
			b.WriteString(ErrorStyle.Render(m.err) + "\n")
		}
	}
	b.WriteString(MutedStyle.Render("↑/↓ move · ←/→ choose · enter on [ run ] runs · esc close"))
	w := m.w - 4
	if w > 90 {
		w = 90
	}
	if w < 40 {
		w = 40
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(w).Render(b.String())
}
