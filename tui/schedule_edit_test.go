package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func editForm() cli.ScheduleForm {
	return cli.ScheduleForm{Cron: "0 9 * * *", Tz: "Asia/Tokyo", Repo: "/r", Agent: "python", AgentArgs: []string{"--x"},
		Prompt: "daily triage", Kind: "oneshot", Record: "on", Deadline: "30m0s", TurnEnd: "any",
		ResumeConversation: true, Caps: "spawn", Scope: "subtree", ScopeFor: []string{}, GrantConditions: []string{},
		DelegateCaps: "none", DelegateScope: "subtree", SpawnDepth: "unbounded", MaxDescendants: "unbounded"}
}

// editAuth is editForm's authority in wire types, as ScheduleFormMsg carries it.
func editAuth(t *testing.T) Authority {
	t.Helper()
	caps, err := cli.ParseCaps("spawn")
	if err != nil {
		t.Fatal(err)
	}
	return Authority{Caps: caps, Scope: protocol.TaskScope{Base: protocol.ScopeBase_Subtree},
		DelegateScope: protocol.TaskScope{Base: protocol.ScopeBase_Subtree}}
}

// The schedule form opened on a schedule holds its values; saved untouched it
// sends nothing; a change sends only itself, in the verb's spelling.
func TestScheduleEditFormSendsOnlyWhatChanged(t *testing.T) {
	id := strings.Repeat("ab", 16)
	m := NewBgForm()
	m.OpenScheduleEdit(id, editForm(), editAuth(t))
	if !m.IsScheduleEdit() || m.fields[bgFieldCron].Value() != "0 9 * * *" || m.prompt.Value() != "daily triage" ||
		m.fields[bgFieldKind].Value() != "oneshot" || m.fields[bgFieldRecord].Value() != "on" {
		t.Fatalf("prefill: cron=%q prompt=%q oneshot=%q record=%q", m.fields[bgFieldCron].Value(), m.prompt.Value(),
			m.fields[bgFieldKind].Value(), m.fields[bgFieldRecord].Value())
	}
	if args, err := m.ScheduleEditArgs(); err != nil || !reflect.DeepEqual(args, []string{id}) {
		t.Fatalf("untouched: %q %v", args, err)
	}
	m.fields[bgFieldCron].SetValue("@daily")
	m.fields[bgFieldKind].SetValue("interactive")
	args, err := m.ScheduleEditArgs()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args[1:], " "); got != "--cron @daily --oneshot=false" {
		t.Fatalf("edit line %q", got)
	}
}

// What an edit cannot change is not in the Tab order; a kind conflict is said.
func TestScheduleEditFormLocksRepoAndTask(t *testing.T) {
	m := NewBgForm()
	m.OpenScheduleEdit(strings.Repeat("ab", 16), editForm(), editAuth(t))
	for _, f := range m.order() {
		if f == bgFieldRepo || f == bgFieldTarget {
			t.Fatalf("field %s is editable in an edit", bgFieldLabels[f])
		}
	}
	m.fields[bgFieldKind].SetValue("oneshot,stream")
	if _, err := m.ScheduleEditArgs(); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("two kinds: %v", err)
	}
	if !strings.Contains(m.View(), "edit schedule") {
		t.Fatalf("title:\n%s", m.View())
	}
}

// E on the schedules table fetches the schedule and opens the form on it.
func TestScheduleFormMsgOpensTheEdit(t *testing.T) {
	a := &App{cmdresult: NewCmdResult(), bgform: NewBgForm()}
	a.Update(ScheduleFormMsg{ID: strings.Repeat("ab", 16), Form: editForm(), Auth: editAuth(t)})
	if !a.bgform.IsOpen() || !a.bgform.IsScheduleEdit() {
		t.Fatal("the edit form did not open")
	}
}

// The authority is a row of the schedule form: a new schedule starts from the
// session defaults and sends them, as a session does; an edit starts from the
// stored one and sends it only when it changed.
func TestScheduleFormCarriesTheAuthority(t *testing.T) {
	f := NewBgForm()
	f.OpenSchedule("/r", editAuth(t))
	f.prompt.SetValue("triage")
	f.fields[bgFieldCron].SetValue("@daily")
	f.fields[bgFieldAgent].SetValue("claude")
	args, err := f.ScheduleArgs(identityResolve)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(args, "--caps", "spawn") || !hasPair(args, "--scope", "subtree") {
		t.Fatalf("new schedule args %q lack the authority", args)
	}
	if _, err := verb.ParseCmdScheduleNew(verb.TUI, args, nil); err != nil {
		t.Fatalf("the shared parser refuses %q: %v", args, err)
	}
	if !strings.Contains(f.View(), "authority") {
		t.Fatalf("no authority row:\n%s", f.View())
	}

	id := strings.Repeat("ab", 16)
	m := NewBgForm()
	m.OpenScheduleEdit(id, editForm(), editAuth(t))
	if args, err := m.ScheduleEditArgs(); err != nil || len(args) != 1 {
		t.Fatalf("untouched authority sent: %q %v", args, err)
	}
	au := m.Authority()
	au.Caps, _ = cli.ParseCaps("spawn,cancel")
	m.SetAuthority(au)
	args, err = m.ScheduleEditArgs()
	if err != nil || !hasPair(args, "--caps", "spawn,cancel") {
		t.Fatalf("changed caps: %q %v", args, err)
	}
	if _, err := verb.ParseCmdScheduleEdit(verb.TUI, args, nil); err != nil {
		t.Fatalf("the shared parser refuses %q: %v", args, err)
	}
}

// Enter on the authority row opens the authority picker on it; Esc there
// returns to the form with the authority as it was.
func TestScheduleFormAuthorityRowOpensThePicker(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	a.bgform.OpenScheduleEdit(strings.Repeat("ab", 16), editForm(), editAuth(t))
	for i := 0; i < 40 && !a.bgform.AuthorityFocused(); i++ {
		a = press(t, a, tea.KeyMsg{Type: tea.KeyTab})
	}
	if !a.bgform.AuthorityFocused() {
		t.Fatal("Tab never reaches the authority row")
	}
	a = press(t, a, tea.KeyMsg{Type: tea.KeyEnter})
	if !a.authorityPicker.IsOpen() || a.authorityPicker.Mode() != PickerModeSchedule || a.bgform.IsOpen() {
		t.Fatalf("picker open=%v mode=%v form open=%v", a.authorityPicker.IsOpen(), a.authorityPicker.Mode(), a.bgform.IsOpen())
	}
	a = press(t, a, tea.KeyMsg{Type: tea.KeyEsc})
	if a.authorityPicker.IsOpen() || !a.bgform.IsOpen() || !a.bgform.IsScheduleEdit() {
		t.Fatalf("after Esc: picker=%v form=%v", a.authorityPicker.IsOpen(), a.bgform.IsOpen())
	}
	if args, err := a.bgform.ScheduleEditArgs(); err != nil || len(args) != 1 {
		t.Fatalf("Esc changed the authority: %q %v", args, err)
	}
}
