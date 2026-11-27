package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

var (
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyUp    = tea.KeyMsg{Type: tea.KeyUp}
	keyRight = tea.KeyMsg{Type: tea.KeyRight}
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
)

func typeKeys(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func deployView() cli.ActionView {
	return cli.ActionView{
		ID: strings.Repeat("a1", 16), Name: "deploy", Revision: 3,
		Params: []cli.ActionParamView{
			{Name: "env", Type: "choice", Choices: []string{"staging", "prod"}},
			{Name: "ref", Type: "token", Choices: []string{}},
		},
		Template: []cli.ActionElementView{
			{Literal: "./deploy.sh"}, {IsParam: true, Param: "env"}, {IsParam: true, Param: "ref"},
		},
		TemplateText: "./deploy.sh {env} {ref}", EditedBy: "operator",
	}
}

func TestActionsModalListsViews(t *testing.T) {
	m := NewActionsModal()
	m.SetSize(100, 30)
	m.Open()
	m.SetViews([]cli.ActionView{deployView()}, nil)
	v := m.View()
	for _, want := range []string{"deploy", "rev=3", "run: ./deploy.sh {env} {ref}", `param env: choice "staging" "prod"`} {
		if !strings.Contains(v, want) {
			t.Errorf("list lacks %q:\n%s", want, v)
		}
	}
	m.SetViews(nil, nil)
	if !strings.Contains(m.View(), "no actions") {
		t.Errorf("an empty list must say so:\n%s", m.View())
	}
}

// The form refuses what the server would, by the server's rule, and the
// program word cannot be turned into a param.
func TestActionFormRefusesWhatTheServerWould(t *testing.T) {
	f := NewActionForm(nil)
	f.form.Name = "x"
	f.form.Params = []cli.ActionParamView{{Name: "env", Type: "choice", Choices: []string{"a"}}}
	f.form.Template = []cli.ActionElementView{{Literal: "run"}}
	f.rebuild()
	f.load() // the form was set directly, not typed: reload the input
	if err := f.Check(); err == "" || !strings.Contains(err, "env") {
		t.Fatalf("an unused param must be refused naming it: %q", err)
	}
	f.focusRow(rowArgKind, 0, 0)
	f.Update(keyRight)
	if f.form.Template[0].IsParam {
		t.Fatal("the program word became a param")
	}
	if f.Update(keyEnter) {
		t.Fatal("submitted with a broken definition")
	}
}

// Edit carries the revision it was opened at; a refusal (stale_revision)
// keeps the form, with the server's text, so nothing typed is lost.
func TestActionFormEditKeepsFormOnRefusal(t *testing.T) {
	m := NewActionsModal()
	m.SetSize(100, 30)
	m.Open()
	m.SetViews([]cli.ActionView{deployView()}, nil)
	m.OpenForm(true)
	if got := m.form.Form(); got.Ref != deployView().ID || got.Revision != 3 {
		t.Fatalf("edit form: %+v", got)
	}
	m.form.form.Description = "typed"
	m.ApplyResult(ActionFormResultMsg{Text: "action edit deploy: stale_revision — reload", OK: false})
	if m.mode != actionsForm || m.form.form.Description != "typed" || !strings.Contains(m.View(), "stale_revision") {
		t.Fatalf("a refusal must keep the form and show why: mode=%v desc=%q\n%s", m.mode, m.form.form.Description, m.View())
	}
	m.ApplyResult(ActionFormResultMsg{Text: "action edit deploy: rev 4", OK: true})
	if m.mode != actionsList {
		t.Fatalf("ok returns to the list, mode=%v", m.mode)
	}
}

func TestActionRunFormNoActions(t *testing.T) {
	r := NewActionRunForm()
	r.OpenFor(strings.Repeat("cd", 16))
	r.SetViews(nil, nil)
	if !strings.Contains(r.View(), "no actions") {
		t.Fatalf("no actions must be said:\n%s", r.View())
	}
	if r.Update(keyEnter) {
		t.Fatal("ran with nothing to run")
	}
}

// A choice param cycles its values, a token param is typed and checked by
// Go's token rule before anything is sent.
func TestActionRunFormChoiceCyclesAndTokenIsChecked(t *testing.T) {
	r := NewActionRunForm()
	r.OpenFor(strings.Repeat("cd", 16))
	r.SetViews([]cli.ActionView{deployView()}, nil)
	r.Update(keyDown)  // env
	r.Update(keyRight) // staging -> prod
	r.Update(keyDown)  // ref
	r.Update(typeKeys("-x"))
	if _, _, err := r.Request(); err == nil || !strings.Contains(err.Error(), "ref") {
		t.Fatalf("a bad token must be refused naming the param: %v", err)
	}
	r.input.SetValue("v1.2")
	name, values, err := r.Request()
	if err != nil || name != "deploy" || values["env"] != "prod" || values["ref"] != "v1.2" {
		t.Fatalf("request: %q %v %v", name, values, err)
	}
}
