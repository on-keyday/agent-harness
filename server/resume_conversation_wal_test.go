package server

import (
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A resume that asked for the agent's conversation and is still Queued when
// the server restarts must come back asking for it: the flag is per-open like
// the prompt, and the prompt already survives. Before task_resumed carried
// it, the replayed task dispatched without --continue, with no error.
func TestResumeConversationSurvivesWALReplay(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "resume-conv.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	s := NewTaskStore()
	s.SetWAL(wal)

	grant := authority.Grant{Caps: protocol.Capability_All, Scope: authority.DefaultScope()}
	noChange := GrantUpdate{To: authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}}
	resume := func(id string, conv bool) {
		t.Helper()
		if _, err := s.Resume(id, "again", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, noChange, protocol.TaskKind_Oneshot, "", protocol.TaskID{}, conv, RunSpec{}); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	finish := func(id string) {
		s.Assign(id, testRunnerID("r"), "/wt", false)
		s.Finish(id, 0, nil)
	}

	withConv := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, grant, "")
	finish(withConv)
	resume(withConv, true) // left Queued: the restart window

	// The latest resume wins: an earlier "with conversation" must not leak
	// into a later fresh one.
	fresh := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, grant, "")
	finish(fresh)
	resume(fresh, true)
	finish(fresh)
	resume(fresh, false)

	wal.Close() //nolint:errcheck
	events, _, err := ReadWAL(walPath)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	s2 := NewTaskStore()
	s2.ReplayEvents(events)

	for _, c := range []struct {
		id   string
		want bool
	}{{withConv, true}, {fresh, false}} {
		got, ok := s2.Get(c.id)
		if !ok {
			t.Fatalf("task %s missing after replay", c.id)
		}
		if got.Status != protocol.TaskStatus_Queued {
			t.Fatalf("task %s status=%v after replay, want Queued", c.id, got.Status)
		}
		if got.ResumeConversation != c.want {
			t.Errorf("task %s ResumeConversation=%v after replay, want %v", c.id, got.ResumeConversation, c.want)
		}
	}
}

// Resume fires OnCreate, which queues the task for the scheduler. The flag
// must already be set when it does: the handlers used to set it in a second
// call after Resume returned, so a dispatch in between ran without it.
func TestResumeConversationSetBeforeOnCreate(t *testing.T) {
	s := NewTaskStore()
	grant := authority.Grant{Caps: protocol.Capability_All, Scope: authority.DefaultScope()}
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, grant, "")
	s.Assign(id, testRunnerID("r"), "/wt", false)
	s.Finish(id, 0, nil)

	var seen, fired bool
	s.OnCreate = func(got string) {
		if got != id {
			return
		}
		e, _ := s.Get(id)
		seen, fired = e.ResumeConversation, true
	}
	if _, err := s.Resume(id, "again", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, GrantUpdate{To: authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}}, protocol.TaskKind_Oneshot, "", protocol.TaskID{}, true, RunSpec{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !fired {
		t.Fatal("OnCreate did not fire on Resume")
	}
	if !seen {
		t.Fatal("ResumeConversation was false when OnCreate fired")
	}
}
