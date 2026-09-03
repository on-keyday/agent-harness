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
