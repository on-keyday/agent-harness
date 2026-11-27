package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

func editForm() cli.ScheduleForm {
	return cli.ScheduleForm{Cron: "0 9 * * *", Tz: "Asia/Tokyo", Repo: "/r", Agent: "python", AgentArgs: []string{"--x"},
		Prompt: "daily triage", Kind: "oneshot", Record: "on", Deadline: "30m0s", TurnEnd: "any",
		ResumeConversation: true, Caps: "spawn", Scope: "subtree", ScopeFor: []string{}, GrantConditions: []string{},
		DelegateCaps: "none", DelegateScope: "subtree", SpawnDepth: "unbounded", MaxDescendants: "unbounded"}
}

// The schedule form opened on a schedule holds its values; saved untouched it
// sends nothing; a change sends only itself, in the verb's spelling.
func TestScheduleEditFormSendsOnlyWhatChanged(t *testing.T) {
	id := strings.Repeat("ab", 16)
	m := NewBgForm()
	m.OpenScheduleEdit(id, editForm())
	if !m.IsScheduleEdit() || m.fields[bgFieldCron].Value() != "0 9 * * *" || m.prompt.Value() != "daily triage" ||
		m.fields[bgFieldOneshot].Value() != "yes" || m.fields[bgFieldRecord].Value() != "on" {
		t.Fatalf("prefill: cron=%q prompt=%q oneshot=%q record=%q", m.fields[bgFieldCron].Value(), m.prompt.Value(),
			m.fields[bgFieldOneshot].Value(), m.fields[bgFieldRecord].Value())
	}
	if args, err := m.ScheduleEditArgs(); err != nil || !reflect.DeepEqual(args, []string{id}) {
		t.Fatalf("untouched: %q %v", args, err)
	}
	m.fields[bgFieldCron].SetValue("@daily")
	m.fields[bgFieldOneshot].SetValue("no")
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
	m.OpenScheduleEdit(strings.Repeat("ab", 16), editForm())
	for _, f := range m.order() {
		if f == bgFieldRepo || f == bgFieldTarget {
			t.Fatalf("field %s is editable in an edit", bgFieldLabels[f])
		}
	}
	m.fields[bgFieldStream].SetValue("yes")
	if _, err := m.ScheduleEditArgs(); err == nil || !strings.Contains(err.Error(), "stream") {
		t.Fatalf("one-shot and stream together: %v", err)
	}
	if !strings.Contains(m.View(), "edit schedule") {
		t.Fatalf("title:\n%s", m.View())
	}
}

// E on the schedules table fetches the schedule and opens the form on it.
func TestScheduleFormMsgOpensTheEdit(t *testing.T) {
	a := &App{cmdresult: NewCmdResult(), bgform: NewBgForm()}
	a.Update(ScheduleFormMsg{ID: strings.Repeat("ab", 16), Form: editForm()})
	if !a.bgform.IsOpen() || !a.bgform.IsScheduleEdit() {
		t.Fatal("the edit form did not open")
	}
}
