package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func conditionedTask(t *testing.T, withBody bool) protocol.TaskInfo {
	t.Helper()
	_, ov, err := verb.ParseScopeFor("forward_local=global")
	if err != nil {
		t.Fatal(err)
	}
	cond, err := verb.ParseGrantCondition("forward_local=ports:8000-8100")
	if err != nil {
		t.Fatal(err)
	}
	marked, err := verb.MarkGrantConditions([]protocol.ScopeOverride{ov}, []protocol.GrantCondition{cond})
	if err != nil {
		t.Fatal(err)
	}
	task := pickerTask(0xaa, "t")
	task.Capabilities = protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote
	task.SetOverrides(marked)
	if withBody {
		task.SetGrantConditions([]protocol.GrantCondition{cond})
	}
	return task
}

// The re-grant picker carries the overrides it does not edit, and their
// conditions with them: a re-grant that dropped them would widen the grant,
// and one that sent the marker alone would be refused (grant conditions C6).
func TestRegrantPickerCarriesGrantConditions(t *testing.T) {
	target := conditionedTask(t, true)
	var m AuthorityPickerModel
	m.OpenRegrant(target, []protocol.TaskInfo{target})
	got := m.GrantConditions()
	if len(got) != 1 || got[0].Caps != protocol.Capability_ForwardLocal {
		t.Fatalf("picker carries %+v, want the target's forward_local condition", got)
	}
	m.OpenSession(Authority{}, nil)
	if len(m.GrantConditions()) != 0 {
		t.Fatal("the session-defaults picker kept a re-grant target's conditions")
	}
}

func TestTaskDetailShowsGrantConditions(t *testing.T) {
	if got := formatTaskDetail(conditionedTask(t, true), nil); !strings.Contains(got, "conditions:    forward_local:ports:8000-8100") {
		t.Fatalf("detail:\n%s", got)
	}
	if got := formatTaskDetail(conditionedTask(t, false), nil); !strings.Contains(got, "forward_local:(condition unknown)") {
		t.Fatalf("detail without the body:\n%s", got)
	}
}
