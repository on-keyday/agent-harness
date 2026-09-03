package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// walStore returns a TaskStore writing to a WAL under a temp dir, plus a
// reader for the events it has appended so far.
func walStore(t *testing.T) (*TaskStore, func() []WALEvent, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	s := NewTaskStore()
	s.SetWAL(w)
	return s, func() []WALEvent {
		events, rerr := ReadWAL(path)
		if rerr != nil {
			t.Fatalf("ReadWAL: %v", rerr)
		}
		return events
	}, dir
}

// TestRestoreBringsBackAPrunedTask is the round trip the whole verb exists
// for: a terminal task, pruned, then restored from the WAL with its fields
// intact.
//
// The reason it exists at all: a prune deletes the TaskEntry, and
// handleSubmitResume's first precondition is that the entry still exists -- so
// before this, a pruned id could not be resumed, and the sweep that removed it
// was reachable by typing the bare verb.
func TestRestoreBringsBackAPrunedTask(t *testing.T) {
	s, events, dir := walStore(t)
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	id := s.Create("/repo", "the prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, []string{"--flag"},
		protocol.Capability_Spawn, defaultScope(), "codex")
	s.MarkFailed(id, "done") // terminal, so the age sweep takes it

	if n := s.PruneTerminal(nil, time.Now().Add(time.Hour), logs); n != 1 {
		t.Fatalf("PruneTerminal removed %d, want 1", n)
	}
	if _, ok := s.Get(id); ok {
		t.Fatal("the task is still in the store after a prune")
	}

	restored, present, missing := s.RestoreFromWAL(events(), []string{id})
	if len(restored) != 1 || len(present) != 0 || len(missing) != 0 {
		t.Fatalf("restored=%v present=%v missing=%v", restored, present, missing)
	}
	got, ok := s.Get(id)
	if !ok {
		t.Fatal("the task is not in the store after a restore")
	}
	// Every field comes from the WAL record the server wrote, so they all
	// have to survive -- a restore that returned a hollow entry would be
	// worse than none, because `ls` would show it and a resume would run it.
	if got.RepoPath != "/repo" || got.Prompt != "the prompt" {
		t.Errorf("repo/prompt lost: %+v", got)
	}
	if got.AgentProfile != "codex" {
		t.Errorf("AgentProfile = %q, want codex", got.AgentProfile)
	}
	if got.Capabilities != protocol.Capability_Spawn {
		t.Errorf("Capabilities = %v, want spawn", got.Capabilities)
	}
	if len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "--flag" {
		t.Errorf("ExtraArgs = %v", got.ExtraArgs)
	}
	if got.Status != protocol.TaskStatus_Failed {
		t.Errorf("Status = %v, want the terminal status it had", got.Status)
	}
	// It is in the listing again, which is what makes it resumable.
	found := false
	for _, e := range s.List(0) {
		if e.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("the restored task is absent from List, so nothing can reach it")
	}
}

// A restore must never overwrite a live task with an older snapshot of it.
func TestRestoreLeavesALiveTaskAlone(t *testing.T) {
	s, events, _ := walStore(t)
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	s.MarkFailed(id, "boom")

	restored, present, missing := s.RestoreFromWAL(events(), []string{id})
	if len(restored) != 0 || len(present) != 1 || len(missing) != 0 {
		t.Fatalf("restored=%v present=%v missing=%v", restored, present, missing)
	}
	got, _ := s.Get(id)
	if got.Status != protocol.TaskStatus_Failed || string(got.ErrorMsg) != "boom" {
		t.Errorf("the live entry was rewritten: %+v", got)
	}
	// And no duplicate row in the listing.
	n := 0
	for _, e := range s.List(0) {
		if e.ID == id {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the id appears %d times in List, want 1", n)
	}
}

// An id the server never wrote cannot be invented.
func TestRestoreRefusesAnIDTheWALNeverSaw(t *testing.T) {
	s, events, _ := walStore(t)
	restored, present, missing := s.RestoreFromWAL(events(), []string{"ff" + "00000000000000000000000000000"})
	if len(restored) != 0 || len(present) != 0 || len(missing) != 1 {
		t.Fatalf("restored=%v present=%v missing=%v", restored, present, missing)
	}
}

// The restore itself is recorded, so a later replay does not undo it: the
// task_pruned that removed the entry is still in the WAL, and a server
// restart replays the whole file in order.
func TestRestoreSurvivesAReplayOfTheWholeWAL(t *testing.T) {
	s, events, dir := walStore(t)
	logs := filepath.Join(dir, "logs")
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	s.MarkFailed(id, "done")
	s.PruneTerminal(nil, time.Now().Add(time.Hour), logs)
	if restored, _, _ := s.RestoreFromWAL(events(), []string{id}); len(restored) != 1 {
		t.Fatal("restore did not put it back")
	}

	fresh := NewTaskStore()
	fresh.ReplayEvents(events())
	if _, ok := fresh.Get(id); !ok {
		t.Fatal("a restart replaying the WAL drops the restored task: " +
			"task_restored must undo the task_pruned that precedes it")
	}
}

// TestRestorableListsWhatAPruneForgot is the half that makes restore usable:
// the verb takes ids, and the ids of forgotten tasks are in a file on the
// server host that no listing reads. Without this an operator who did not
// write the id down before pruning -- which is everyone who pruned by
// accident -- could not learn it.
func TestRestorableListsWhatAPruneForgot(t *testing.T) {
	s, events, dir := walStore(t)
	logs := filepath.Join(dir, "logs")
	a := s.Create("/repo", "first", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	b := s.Create("/repo", "second", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	live := s.Create("/repo", "still here", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	s.MarkFailed(a, "x")
	s.MarkFailed(b, "x")
	s.PruneByIDs(nil, []string{a, b}, false, logs)

	rows := RestorableFromWAL(events(), s.Live)
	if len(rows) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(rows), rows)
	}
	byID := map[string]Restorable{}
	for _, r := range rows {
		byID[r.TaskID] = r
		if r.TaskID == live {
			t.Error("a live task is listed as restorable")
		}
		// The fields that make an id identifiable without one.
		if r.PrunedAt.IsZero() {
			t.Errorf("%s: no prune time, so an operator cannot tell which sweep took it", r.TaskID)
		}
		if r.RepoPath != "/repo" {
			t.Errorf("%s: repo lost", r.TaskID)
		}
	}
	if byID[a].Prompt != "first" || byID[b].Prompt != "second" {
		t.Errorf("prompts lost: %+v", rows)
	}

	// A restored id leaves the list, by the same rule the replay uses.
	if restored, _, _ := s.RestoreFromWAL(events(), []string{a}); len(restored) != 1 {
		t.Fatal("restore failed")
	}
	rows = RestorableFromWAL(events(), s.Live)
	if len(rows) != 1 || rows[0].TaskID != b {
		t.Fatalf("after restoring %s the list is %+v", a, rows)
	}
}

// TestRestoreScopeSeesThroughTheHoleAPruneLeft is the reason restore is
// scope-gated rather than operator-only.
//
// An agent may only prune within its scope, so an agent that could not restore
// there could not undo its own mistake -- and the accident this verb exists
// for is exactly the one an agent has. The gate is prune's own bit and target
// set, which grants no reach: the ids it can name are ids it could already
// have pruned.
//
// The hard part is that childIndex is built from the LIVE store, so a pruned
// task has no parent link there and the subtree walk stops at the hole. Its
// creator edge survives only in task_created, which walChildIndex feeds back
// in -- and this asserts that, not the plumbing: a grandchild that was pruned
// must still be in its grandparent's subtree.
func TestRestoreScopeSeesThroughTheHoleAPruneLeft(t *testing.T) {
	h, p, c, g, u := scopeFixture(t)
	dir := t.TempDir()
	w, err := OpenWAL(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// The fixture's tasks pre-date the WAL, so their creator edges are written
	// here as the task_created records a real run would have appended.
	for _, e := range []struct{ id, creator string }{{p, ""}, {c, p}, {g, c}, {u, ""}} {
		if err := w.Write(WALEvent{Type: "task_created", TaskID: e.id, CreatorTaskID: e.creator}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := ReadWAL(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	h.RestoreEventsFn = func() []WALEvent { return events }

	// Prune the grandchild, so it is gone from the live store and its edge
	// exists only in the WAL.
	h.Tasks.MarkFailed(g, "done")
	if n, _, _ := h.Tasks.PruneByIDs(nil, []string{g}, false, ""); n != 1 {
		t.Fatalf("PruneByIDs removed %d, want 1", n)
	}

	cid := bindPrincipal(t, h, p)
	all, allowed := h.restoreScope(cid)
	if all {
		t.Fatal("an agent principal must not get an unrestricted restore set")
	}
	if !allowed[g] {
		t.Error("the pruned grandchild is outside its grandparent's restore scope — " +
			"the subtree walk stopped at the hole the prune left")
	}
	if !allowed[c] {
		t.Error("the live child fell out of the set")
	}
	if allowed[u] {
		t.Error("an unrelated task is inside the restore scope")
	}
}

// The scope must actually narrow: an agent may not restore a stranger's task.
func TestRestoreScopeExcludesAStranger(t *testing.T) {
	h, p, _, _, u := scopeFixture(t)
	h.RestoreEventsFn = func() []WALEvent {
		return []WALEvent{{Type: "task_created", TaskID: u}}
	}
	cid := bindPrincipal(t, h, p)
	_, allowed := h.restoreScope(cid)
	if allowed[u] {
		t.Error("a task created by nobody in this subtree is restorable by it")
	}
}

// An operator still gets everything: scopeSet answers all=true for a caller
// with no principal task, and restore rides that unchanged.
func TestRestoreScopeIsUnrestrictedForAnOperator(t *testing.T) {
	h, _, _, _, _ := scopeFixture(t)
	h.RestoreEventsFn = func() []WALEvent { return nil }
	all, allowed := h.restoreScope("ws:127.0.0.1:1-operator")
	if !all || allowed != nil {
		t.Fatalf("operator restore scope: all=%v allowed=%v, want unrestricted", all, allowed)
	}
}
