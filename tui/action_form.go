package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// ActionFormModel is the define / edit form of the Actions modal: the TUI's
// counterpart of the WebUI's, over the same cli.ActionForm. It is a column of
// rows — name, description, each param (name, type, its choices), each
// argument of the command (literal or which param, and the literal's text) —
// moved through with ↑/↓. Typing edits a text row, ←/→ cycles a type or kind
// row, enter on a "+" row adds, ctrl+d removes the row's item. The check is
// the server's own (cli.ActionPutInputFromForm), so the form refuses what the
// server would without restating a rule.
type ActionFormModel struct {
	form  cli.ActionForm
	rows  []formRow
	cur   int
	input textinput.Model
	err   string // a refusal from the server, shown until the next edit
}

type formRowKind int

const (
	rowName formRowKind = iota
	rowDesc
	rowParamName
	rowParamType
	rowChoice
	rowAddChoice
	rowAddParam
	rowArgKind
	rowArgLiteral
	rowAddArg
	rowSubmit
)

// formRow is one line of the form: its kind, the param or argument index p,
// and the choice index c.
type formRow struct {
	kind formRowKind
	p, c int
}

func (r formRow) isText() bool {
	return r.kind == rowName || r.kind == rowDesc || r.kind == rowParamName || r.kind == rowChoice || r.kind == rowArgLiteral
}

// NewActionForm is a new action (v nil: one literal word, no params) or an
// edit of v at its revision.
func NewActionForm(v *cli.ActionView) ActionFormModel {
	in := textinput.New()
	in.Width = 40
	in.CharLimit = 4096
	m := ActionFormModel{input: in}
	if v == nil {
		m.form = cli.ActionForm{Template: []cli.ActionElementView{{}}}
	} else {
		m.form = cli.ActionForm{Ref: v.ID, Revision: v.Revision, Name: v.Name, Description: v.Description}
		for _, p := range v.Params {
			m.form.Params = append(m.form.Params, cli.ActionParamView{Name: p.Name, Type: p.Type, Choices: append([]string{}, p.Choices...)})
		}
		m.form.Template = append([]cli.ActionElementView{}, v.Template...)
	}
	m.rebuild()
	m.load()
	return m
}

// Form is what the form submits.
func (m *ActionFormModel) Form() cli.ActionForm {
	m.store()
	return m.form
}

// Check is "" when the form can be sent, else the first rule it breaks.
func (m *ActionFormModel) Check() string {
	if _, err := cli.ActionPutInputFromForm(m.Form()); err != nil {
		return err.Error()
	}
	return ""
}

func (m *ActionFormModel) rebuild() {
	m.rows = []formRow{{kind: rowName}, {kind: rowDesc}}
	for i, p := range m.form.Params {
		m.rows = append(m.rows, formRow{kind: rowParamName, p: i}, formRow{kind: rowParamType, p: i})
		if p.Type == "choice" {
			for j := range p.Choices {
				m.rows = append(m.rows, formRow{kind: rowChoice, p: i, c: j})
			}
			m.rows = append(m.rows, formRow{kind: rowAddChoice, p: i})
		}
	}
	m.rows = append(m.rows, formRow{kind: rowAddParam})
	for i, e := range m.form.Template {
		m.rows = append(m.rows, formRow{kind: rowArgKind, p: i})
		if !e.IsParam {
			m.rows = append(m.rows, formRow{kind: rowArgLiteral, p: i})
		}
	}
	m.rows = append(m.rows, formRow{kind: rowAddArg}, formRow{kind: rowSubmit})
	if m.cur >= len(m.rows) {
		m.cur = len(m.rows) - 1
	}
}

// focusRow moves to the first row of kind for p (and c).
func (m *ActionFormModel) focusRow(kind formRowKind, p, c int) {
	m.store()
	for i, r := range m.rows {
		if r.kind == kind && r.p == p && r.c == c {
			m.cur = i
			break
		}
	}
	m.load()
}

// text is the model string a text row edits.
func (m *ActionFormModel) text(r formRow) *string {
	switch r.kind {
	case rowName:
		return &m.form.Name
	case rowDesc:
		return &m.form.Description
	case rowParamName:
		return &m.form.Params[r.p].Name
	case rowChoice:
		return &m.form.Params[r.p].Choices[r.c]
	case rowArgLiteral:
		return &m.form.Template[r.p].Literal
	}
	return nil
}

// store writes the input back to the row it edits. A param renamed carries
// its references in the command with it.
func (m *ActionFormModel) store() {
	if m.cur >= len(m.rows) || !m.rows[m.cur].isText() {
		return
	}
	r := m.rows[m.cur]
	s := m.text(r)
	if r.kind == rowParamName && *s != m.input.Value() {
		for i := range m.form.Template {
			if m.form.Template[i].IsParam && m.form.Template[i].Param == *s {
				m.form.Template[i].Param = m.input.Value()
			}
		}
	}
	*s = m.input.Value()
}

func (m *ActionFormModel) load() {
	if m.cur < len(m.rows) && m.rows[m.cur].isText() {
		m.input.SetValue(*m.text(m.rows[m.cur]))
		m.input.CursorEnd()
		m.input.Focus()
		return
	}
	m.input.Blur()
}

// argKinds is what an argument row can be: a literal, or one of the params.
func (m *ActionFormModel) argKinds() []string {
	out := []string{""}
	for _, p := range m.form.Params {
		out = append(out, p.Name)
	}
	return out
}

// Update applies one key. It reports true when the operator submitted a form
// that passes Check.
func (m *ActionFormModel) Update(msg tea.KeyMsg) bool {
	r := m.rows[m.cur]
	switch msg.String() {
	case "up", "shift+tab":
		m.store()
		if m.cur > 0 {
			m.cur--
		}
		m.load()
		return false
	case "down", "tab":
		m.store()
		if m.cur < len(m.rows)-1 {
			m.cur++
		}
		m.load()
		return false
	case "ctrl+d":
		m.store()
		m.removeAt(r)
		m.rebuild()
		m.load()
		return false
	}
	m.err = ""
	switch r.kind {
	case rowParamType:
		if msg.String() == "left" || msg.String() == "right" || msg.String() == " " {
			p := &m.form.Params[r.p]
			if p.Type == "choice" {
				p.Type, p.Choices = "token", nil
			} else {
				p.Type, p.Choices = "choice", []string{""}
			}
			m.rebuild()
		}
	case rowArgKind:
		if r.p == 0 { // the program is always written out (C17)
			return false
		}
		if msg.String() == "left" || msg.String() == "right" {
			kinds := m.argKinds()
			e := &m.form.Template[r.p]
			at := 0
			for i, k := range kinds {
				if e.IsParam && k == e.Param {
					at = i
				}
			}
			if msg.String() == "right" {
				at = (at + 1) % len(kinds)
			} else {
				at = (at + len(kinds) - 1) % len(kinds)
			}
			if kinds[at] == "" {
				*e = cli.ActionElementView{}
			} else {
				*e = cli.ActionElementView{IsParam: true, Param: kinds[at]}
			}
			m.rebuild()
		}
	case rowAddChoice, rowAddParam, rowAddArg:
		if msg.Type == tea.KeyEnter {
			switch r.kind {
			case rowAddChoice:
				m.form.Params[r.p].Choices = append(m.form.Params[r.p].Choices, "")
				m.rebuild()
				m.focusRow(rowChoice, r.p, len(m.form.Params[r.p].Choices)-1)
			case rowAddParam:
				m.form.Params = append(m.form.Params, cli.ActionParamView{Type: "token"})
				m.rebuild()
				m.focusRow(rowParamName, len(m.form.Params)-1, 0)
			case rowAddArg:
				m.form.Template = append(m.form.Template, cli.ActionElementView{})
				m.rebuild()
				m.focusRow(rowArgLiteral, len(m.form.Template)-1, 0)
			}
		}
	case rowSubmit:
		return msg.Type == tea.KeyEnter && m.Check() == ""
	default:
		if r.isText() {
			m.input, _ = m.input.Update(msg)
			m.store()
		}
	}
	return false
}

// removeAt drops the param, choice or argument the row stands for. The
// program word stays; a removed param's references become empty literals.
func (m *ActionFormModel) removeAt(r formRow) {
	switch r.kind {
	case rowParamName, rowParamType:
		name := m.form.Params[r.p].Name
		m.form.Params = append(m.form.Params[:r.p], m.form.Params[r.p+1:]...)
		for i := range m.form.Template {
			if m.form.Template[i].IsParam && m.form.Template[i].Param == name {
				m.form.Template[i] = cli.ActionElementView{}
			}
		}
	case rowChoice:
		p := &m.form.Params[r.p]
		p.Choices = append(p.Choices[:r.c], p.Choices[r.c+1:]...)
	case rowArgKind, rowArgLiteral:
		if r.p > 0 {
			m.form.Template = append(m.form.Template[:r.p], m.form.Template[r.p+1:]...)
		}
	}
}

// View renders the form; the current row is marked, and the server's or the
// check's objection is the last line.
func (m *ActionFormModel) View() string {
	var b strings.Builder
	for i, r := range m.rows {
		mark := "  "
		if i == m.cur {
			mark = "▸ "
		}
		val := func(s string) string {
			if i == m.cur && r.isText() {
				return m.input.View()
			}
			return s
		}
		var line string
		switch r.kind {
		case rowName:
			line = "name         " + val(m.form.Name)
		case rowDesc:
			line = "description  " + val(m.form.Description)
		case rowParamName:
			line = fmt.Sprintf("param %d      ", r.p+1) + val(m.form.Params[r.p].Name)
		case rowParamType:
			line = "  type       ◂ " + m.form.Params[r.p].Type + " ▸"
		case rowChoice:
			line = "  choice     " + val(m.form.Params[r.p].Choices[r.c])
		case rowAddChoice:
			line = "  ＋ choice"
		case rowAddParam:
			line = "＋ param"
		case rowArgKind:
			e := m.form.Template[r.p]
			kind := "literal"
			if e.IsParam {
				kind = "{" + e.Param + "}"
			}
			if r.p == 0 {
				line = "arg 0        literal (the program)"
			} else {
				line = fmt.Sprintf("arg %-2d       ◂ %s ▸", r.p, kind)
			}
		case rowArgLiteral:
			line = "  text       " + val(m.form.Template[r.p].Literal)
		case rowAddArg:
			line = "＋ argument"
		case rowSubmit:
			line = "[ save ]"
		}
		b.WriteString(mark + line + "\n")
	}
	switch {
	case m.err != "":
		b.WriteString(ErrorStyle.Render(m.err) + "\n")
	default:
		if e := m.Check(); e != "" {
			b.WriteString(WarnStyle.Render(e) + "\n")
		}
	}
	b.WriteString(MutedStyle.Render("↑/↓ move · type to edit · ←/→ type/kind · enter adds / saves · ctrl+d removes · esc back"))
	return b.String()
}
