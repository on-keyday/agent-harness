package tui

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// liveSessionTask builds a Detached interactive task with the given hex id.
// Named apart from skills_marker_test.go's liveTask, which fills a different
// shape for the Obs column's tests.
func liveSessionTask(t *testing.T, idHex string) protocol.TaskInfo {
	t.Helper()
	raw, err := hex.DecodeString(idHex)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad fixture id %q: %v", idHex, err)
	}
	var ti protocol.TaskInfo
	copy(ti.Id.Id[:], raw)
	ti.Status = protocol.TaskStatus_Detached
	ti.Kind = protocol.TaskKind_Interactive
	return ti
}

// With no existing workspace, every live session starts ticked — that is the
// sensible default — but the operator can untick, which is the whole point of
// the picker existing.
func TestWorkspacePickerDefaultsToLiveSessions(t *testing.T) {
	var m WorkspacePickerModel
	m.Open("mine", []protocol.TaskInfo{liveSessionTask(t, wsTaskA), liveSessionTask(t, wsTaskB)}, nil, nil, nil, "", false, "", false)
	tasks, observed := m.Result()
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want both live sessions ticked", len(tasks))
	}
	if len(observed) != 2 {
		t.Errorf("observed = %v, want both listed rows", observed)
	}
	m.Toggle() // untick the focused row
	tasks, observed = m.Result()
	if len(tasks) != 1 {
		t.Errorf("after untick: len(tasks) = %d, want 1", len(tasks))
	}
	if len(observed) != 2 {
		t.Errorf("an unticked row must still be OBSERVED (so its block is dropped), got %v", observed)
	}
	if len(m.ExcludedIDs()) != 1 {
		t.Errorf("ExcludedIDs = %v, want the unticked one", m.ExcludedIDs())
	}
}

// An existing workspace's own tasks are what a save defaults to, and its
// hand-edited policy is where the picker starts — not the defaults.
func TestWorkspacePickerStartsFromTheExistingWorkspace(t *testing.T) {
	var m WorkspacePickerModel
	existing := &workspace.Workspace{Name: "mine", Tasks: []workspace.Task{
		{ID: wsTaskB, Resume: workspace.ResumeFresh, Runner: workspace.RunnerAny},
	}}
	m.Open("mine", []protocol.TaskInfo{liveSessionTask(t, wsTaskA)}, nil, existing, nil, "", false, "", false)

	tasks, _ := m.Result()
	if len(tasks) != 1 || tasks[0].ID != wsTaskB {
		t.Fatalf("tasks = %+v, want only the already-declared one ticked", tasks)
	}
	if tasks[0].Resume != workspace.ResumeFresh || tasks[0].Runner != workspace.RunnerAny {
		t.Errorf("hand-edited policy was not carried into the picker: %+v", tasks[0])
	}
	// The live session that is NOT declared is listed and unticked, so it can
	// be added without editing the file.
	if len(m.ExcludedIDs()) != 1 || m.ExcludedIDs()[0] != wsTaskA {
		t.Errorf("the undeclared live session should be listed unticked, got %v", m.ExcludedIDs())
	}
}

// A task the workspace declares but which is no longer running must still be
// listed: dropping it has to be a decision, not a consequence of it being down
// at the moment of the save.
func TestWorkspacePickerListsADeclaredButDeadTask(t *testing.T) {
	var m WorkspacePickerModel
	existing := &workspace.Workspace{Name: "mine", Tasks: []workspace.Task{{ID: wsTaskB, Resume: workspace.ResumeContinue}}}
	m.Open("mine", nil, nil, existing, nil, "", false, "", false)
	tasks, observed := m.Result()
	if len(tasks) != 1 || tasks[0].ID != wsTaskB {
		t.Errorf("a declared-but-dead task was not listed/ticked: %+v", tasks)
	}
	if !observed[wsTaskB] {
		t.Error("it must be observed too")
	}
}

func TestWorkspacePickerCycles(t *testing.T) {
	var m WorkspacePickerModel
	m.Open("mine", []protocol.TaskInfo{liveSessionTask(t, wsTaskA)}, nil, nil, nil, "", false, "", false)
	if got := m.rows[0].Resume; got != workspace.ResumeContinue {
		t.Fatalf("initial resume = %q", got)
	}
	m.CycleResume()
	if got := m.rows[0].Resume; got != workspace.ResumeFresh {
		t.Errorf("continue → %q, want fresh", got)
	}
	m.CycleResume()
	if got := m.rows[0].Resume; got != workspace.ResumeNo {
		t.Errorf("fresh → %q, want no", got)
	}
	m.CycleRunner()
	if got := m.rows[0].Runner; got != workspace.RunnerAny {
		t.Errorf("assigned → %q, want any", got)
	}
	m.SetAll(false)
	if tasks, _ := m.Result(); len(tasks) != 0 {
		t.Errorf("SetAll(false) left %d ticked", len(tasks))
	}
}

// A finished task must be OFFERABLE even when it is not already declared:
// "this one is done, bring it back next time" is what resume is for, and the
// first picker could express it only by hand-editing the file.
func TestWorkspacePickerOffersResumableTasksUnticked(t *testing.T) {
	var m WorkspacePickerModel
	done := liveSessionTask(t, wsTaskB)
	done.Status = protocol.TaskStatus_Succeeded
	m.Open("mine", []protocol.TaskInfo{liveSessionTask(t, wsTaskA)}, []protocol.TaskInfo{done}, nil, nil, "", false, "", false)

	if len(m.rows) != 2 {
		t.Fatalf("len(rows) = %d, want the live one and the finished one", len(m.rows))
	}
	tasks, _ := m.Result()
	if len(tasks) != 1 || tasks[0].ID != wsTaskA {
		t.Errorf("a finished task must start UNticked: %+v", tasks)
	}
	// …and ticking it is all it takes to have it resumed on the next start.
	m.Move(1)
	m.Toggle()
	tasks, _ = m.Result()
	if len(tasks) != 2 {
		t.Errorf("ticking the finished task did not include it: %+v", tasks)
	}
}

// The finished tail is capped, and the count that did not fit is reported
// rather than silently dropped.
func TestWorkspacePickerCapsAndReportsTheResumableTail(t *testing.T) {
	var m WorkspacePickerModel
	var many []protocol.TaskInfo
	for i := 0; i < maxResumableRows+7; i++ {
		id := fmt.Sprintf("%032x", i+1)
		ti := liveSessionTask(t, id)
		ti.Status = protocol.TaskStatus_Succeeded
		many = append(many, ti)
	}
	m.Open("mine", nil, many, nil, nil, "", false, "", false)
	if len(m.rows) != maxResumableRows {
		t.Errorf("len(rows) = %d, want the cap %d", len(m.rows), maxResumableRows)
	}
	if m.resumableTotal != maxResumableRows+7 {
		t.Errorf("resumableTotal = %d, want every candidate counted", m.resumableTotal)
	}
	m.SetSize(200, 60)
	if v := m.View(); !strings.Contains(v, "7 more finished task(s) not listed") {
		t.Errorf("the cap is not reported:\n%s", v)
	}
}

// A long list must not push the picker's own title and footer off a short
// terminal — the `?` popup shipped exactly that defect once.
func TestWorkspacePickerFitsAShortTerminal(t *testing.T) {
	var m WorkspacePickerModel
	var live []protocol.TaskInfo
	for i := 0; i < 40; i++ {
		live = append(live, liveSessionTask(t, fmt.Sprintf("%032x", i+1)))
	}
	m.Open("mine", live, nil, nil, nil, "", false, "", false)
	m.SetSize(200, 24)
	lines := strings.Count(m.View(), "\n") + 1
	if lines > 24 {
		t.Errorf("View is %d lines on a 24-row terminal", lines)
	}
	if !strings.Contains(m.View(), "which tasks?") {
		t.Error("the title was pushed out of the box")
	}
	if !strings.Contains(m.View(), "enter save") {
		t.Error("the footer was pushed out of the box")
	}
	// The cursor must stay visible when it moves to the end.
	for i := 0; i < 39; i++ {
		m.Move(1)
	}
	if !strings.Contains(m.View(), "> ") {
		t.Error("the cursor scrolled out of the window")
	}
}

// The grid setting is chosen HERE, not read from a.grid.IsOpen(). The grid is a
// full-screen overlay that intercepts every key, so the command line is
// unreachable while it is open and IsOpen() is always false by the time a save
// runs — gating on it made the grid unsavable in principle.
func TestWorkspacePickerGridChoice(t *testing.T) {
	// No grid this session, none in the file: the only honest state is none.
	var m WorkspacePickerModel
	m.Open("mine", nil, nil, nil, nil, "", false, "", false)
	if _, set, keep := m.GridResult(); set || keep {
		t.Errorf("no grid anywhere should mean none, got set=%v keep=%v", set, keep)
	}
	m.CycleGrid() // none → keep (set is skipped: nothing to write)
	if _, set, keep := m.GridResult(); set || !keep {
		t.Errorf("cycle from none should reach keep, got set=%v keep=%v", set, keep)
	}

	// A grid was opened this session and the file has none: offer to write it.
	m.Open("mine", nil, nil, nil, nil, "--under "+wsTaskA, true, "", false)
	v, set, keep := m.GridResult()
	if !set || keep || v != "--under "+wsTaskA {
		t.Errorf("a session grid with none in the file should default to set: %q %v %v", v, set, keep)
	}
	m.CycleGrid()
	if _, set, keep := m.GridResult(); set || keep {
		t.Errorf("set → none expected, got set=%v keep=%v", set, keep)
	}

	// The file already declares one: default to leaving it alone.
	existing := &workspace.Workspace{Name: "mine", Grid: "--under " + wsTaskB, GridSet: true}
	m.Open("mine", nil, nil, existing, nil, "--under "+wsTaskA, true, "", false)
	if _, set, keep := m.GridResult(); set || !keep {
		t.Errorf("an existing grid line should default to keep, got set=%v keep=%v", set, keep)
	}
	if !strings.Contains(m.gridRowLabel(), "unchanged") {
		t.Errorf("the row should say it is unchanged: %q", m.gridRowLabel())
	}
	m.CycleGrid()
	if v, set, _ := m.GridResult(); !set || v != "--under "+wsTaskA {
		t.Errorf("keep → set should offer THIS session's selection, got %q", v)
	}
}

// Forwards were REAL-TIME ONLY: the list came from the registry and nothing
// could add to it, so "-L 3000 next time" for a task that is not running could
// only be said by editing the file. The editor parses through the same parser
// the command line uses, so it cannot write a spec that would be rejected.
func TestWorkspacePickerForwardEditor(t *testing.T) {
	var m WorkspacePickerModel
	m.Open("mine", []protocol.TaskInfo{liveSessionTask(t, wsTaskA)}, nil, nil,
		map[string][]string{wsTaskA: {"-L 3000:127.0.0.1:3000"}}, "", false, "", false)

	m.BeginEdit()
	if !m.IsEditing() {
		t.Fatal("BeginEdit did not open the editor")
	}
	if got := m.input.Value(); got != "-L 3000:127.0.0.1:3000" {
		t.Errorf("the editor should start from what is there, got %q", got)
	}

	m.input.SetValue("-L 3000:127.0.0.1:3000, -R 8080:127.0.0.1:9090")
	if err := m.CommitEdit(); err != nil {
		t.Fatalf("CommitEdit: %v", err)
	}
	tasks, _ := m.Result()
	if len(tasks[0].Forwards) != 2 || tasks[0].Forwards[1] != "-R 8080:127.0.0.1:9090" {
		t.Errorf("forwards = %q", tasks[0].Forwards)
	}

	// A spec the command line would reject is refused here too, and the row is
	// left as it was rather than half-written.
	m.BeginEdit()
	m.input.SetValue("-W nonsense")
	if err := m.CommitEdit(); err == nil {
		t.Error("a -W value was accepted")
	}
	if !m.IsEditing() {
		t.Error("a rejected edit must stay open so it can be corrected")
	}
	tasks, _ = m.Result()
	if len(tasks[0].Forwards) != 2 {
		t.Errorf("a rejected edit changed the row: %q", tasks[0].Forwards)
	}

	// Emptying the line clears the task's forwards.
	m.input.SetValue("")
	if err := m.CommitEdit(); err != nil {
		t.Fatal(err)
	}
	tasks, _ = m.Result()
	if len(tasks[0].Forwards) != 0 {
		t.Errorf("clearing left %q", tasks[0].Forwards)
	}
}

// The ssh-gateway row seeds from the same three facts the grid row does, and
// the precedence matters: what the FILE says wins over what happens to be
// running, so opening the picker and pressing enter never silently rewrites a
// line the operator put there by hand.
func TestPickerGatewaySeeding(t *testing.T) {
	declared := &workspace.Workspace{Name: "mine", SSHGateway: "127.0.0.1:2222", SSHGatewaySet: true}
	for _, tc := range []struct {
		name      string
		existing  *workspace.Workspace
		gwAddr    string
		gwHave    bool
		wantLabel string
	}{
		{"file wins over what is running", declared, "0.0.0.0:2200", true, "127.0.0.1:2222"},
		{"running and undeclared offers to write it", nil, "0.0.0.0:2200", true, "0.0.0.0:2200"},
		{"neither", nil, "", false, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m WorkspacePickerModel
			m.Open("mine", nil, nil, tc.existing, nil, "", false, tc.gwAddr, tc.gwHave)
			if got := m.gwRowLabel(); !strings.Contains(got, tc.wantLabel) {
				t.Errorf("row = %q, want it to mention %q", got, tc.wantLabel)
			}
		})
	}
}

// `set` must not be offered when there is no gateway to write down — cycling
// into a state that writes nothing is the trap CycleGrid's comment names.
func TestPickerGatewayCycleSkipsSetWithNothingRunning(t *testing.T) {
	var m WorkspacePickerModel
	declared := &workspace.Workspace{Name: "mine", SSHGateway: "127.0.0.1:2222", SSHGatewaySet: true}
	m.Open("mine", nil, nil, declared, nil, "", false, "", false)

	// keep -> (no set available) -> none -> keep
	if _, set, keep := m.GatewayResult(); set || !keep {
		t.Fatalf("initial state = set:%v keep:%v, want keep", set, keep)
	}
	m.CycleGateway()
	if _, set, keep := m.GatewayResult(); set || keep {
		t.Errorf("after one cycle = set:%v keep:%v, want none", set, keep)
	}
	m.CycleGateway()
	if _, _, keep := m.GatewayResult(); !keep {
		t.Error("cycling did not come back to keep")
	}
}

// With one running, `set` carries its address — the value a save writes.
func TestPickerGatewayResultCarriesTheRunningAddress(t *testing.T) {
	var m WorkspacePickerModel
	m.Open("mine", nil, nil, nil, nil, "", false, "0.0.0.0:2200", true)
	val, set, keep := m.GatewayResult()
	if !set || keep || val != "0.0.0.0:2200" {
		t.Errorf("GatewayResult = %q set:%v keep:%v, want the running address as a set", val, set, keep)
	}
}
