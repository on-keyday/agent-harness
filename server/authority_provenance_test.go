package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// provenanceStore is a store writing a WAL, so a test can replay what it wrote.
func provenanceStore(t *testing.T) (*TaskStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provenance.log")
	wal, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	s := NewTaskStore()
	s.SetWAL(wal)
	t.Cleanup(func() { wal.Close() }) //nolint:errcheck
	return s, path
}

func createTask(s *TaskStore, creator protocol.TaskID) string {
	return s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, creator, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_All, authority.Scope{}, "")
}

func wantProvenance(t *testing.T, s *TaskStore, id string, src protocol.AuthoritySource, actor protocol.TaskID) {
	t.Helper()
	e, ok := s.Get(id)
	if !ok {
		t.Fatalf("%s: not in the store", id[:8])
	}
	if e.AuthoritySource != src || e.AuthorityActor != actor || e.AuthorityAt.IsZero() {
		t.Errorf("%s: provenance = %v actor %x at %v, want %v actor %x and a time",
			id[:8], e.AuthoritySource, e.AuthorityActor.Id[:4], e.AuthorityAt, src, actor.Id[:4])
	}
}

func TestProvenanceCreate(t *testing.T) {
	s, _ := provenanceStore(t)
	p := createTask(s, protocol.TaskID{})
	wantProvenance(t, s, p, protocol.AuthoritySource_OperatorSpawn, protocol.TaskID{})
	c := createTask(s, taskIDFromHex(p))
	wantProvenance(t, s, c, protocol.AuthoritySource_Spawn, taskIDFromHex(p))
}

func TestProvenanceSetCaps(t *testing.T) {
	s, _ := provenanceStore(t)
	id := createTask(s, protocol.TaskID{})
	before, _ := s.Get(id)
	s.SetCaps(id, true, protocol.Capability_Cancel, false, authority.Scope{},
		AuthorityWrite{Source: protocol.AuthoritySource_OperatorCapsSet})
	wantProvenance(t, s, id, protocol.AuthoritySource_OperatorCapsSet, protocol.TaskID{})
	if after, _ := s.Get(id); !after.AuthorityAt.After(before.AuthorityAt) && !after.AuthorityAt.Equal(before.AuthorityAt) {
		t.Errorf("AuthorityAt went backwards: %v → %v", before.AuthorityAt, after.AuthorityAt)
	}
}

func TestProvenanceResume(t *testing.T) {
	s, _ := provenanceStore(t)
	id := createTask(s, protocol.TaskID{})
	resumer := taskIDFromHex(createTask(s, protocol.TaskID{}))
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Agent,
		true, protocol.Capability_FileRead, false, authority.Scope{}, protocol.TaskKind_Oneshot, "", resumer); err != nil {
		t.Fatalf("resume: %v", err)
	}
	wantProvenance(t, s, id, protocol.AuthoritySource_Resume, resumer)

	// A resume that keeps the recorded authority writes nothing.
	kept, _ := s.Get(id)
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p3", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
		false, 0, false, authority.Scope{}, protocol.TaskKind_Oneshot, "", protocol.TaskID{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if e, _ := s.Get(id); e.AuthoritySource != kept.AuthoritySource || e.AuthorityActor != kept.AuthorityActor || !e.AuthorityAt.Equal(kept.AuthorityAt) {
		t.Errorf("a keeping resume rewrote provenance: %v/%x/%v → %v/%x/%v",
			kept.AuthoritySource, kept.AuthorityActor.Id[:2], kept.AuthorityAt, e.AuthoritySource, e.AuthorityActor.Id[:2], e.AuthorityAt)
	}
}

func TestProvenanceReparent(t *testing.T) {
	s, _ := provenanceStore(t)
	p := createTask(s, protocol.TaskID{})
	c := createTask(s, taskIDFromHex(p))
	other := createTask(s, protocol.TaskID{})
	s.SetParent(c, taskIDFromHex(other))
	wantProvenance(t, s, c, protocol.AuthoritySource_Reparent, protocol.TaskID{})

	g := createTask(s, taskIDFromHex(c))
	if _, _, err := s.SwapWithParent(g); err != nil {
		t.Fatalf("swap: %v", err)
	}
	// Both tasks of a swap had their parent changed.
	wantProvenance(t, s, g, protocol.AuthoritySource_Reparent, protocol.TaskID{})
	wantProvenance(t, s, c, protocol.AuthoritySource_Reparent, protocol.TaskID{})
}

// What was written survives a restart, and a record from before this field
// replays as unknown.
func TestProvenanceSurvivesReplay(t *testing.T) {
	s, path := provenanceStore(t)
	p := createTask(s, protocol.TaskID{})
	c := createTask(s, taskIDFromHex(p))
	g := createTask(s, taskIDFromHex(c))
	s.SetCaps(c, true, protocol.Capability_Cancel, false, authority.Scope{},
		AuthorityWrite{Source: protocol.AuthoritySource_Cascade, Actor: taskIDFromHex(p)})
	s.SetParent(g, taskIDFromHex(p))

	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	var legacy WALEvent
	if err := json.Unmarshal([]byte(`{"type":"task_created","task_id":"`+
		"0123456789abcdef0123456789abcdef"+`","repo_path":"/r","ts":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	events = append(events, legacy)

	fresh := NewTaskStore()
	fresh.ReplayEvents(events)
	for _, id := range []string{p, c, g} {
		want, _ := s.Get(id)
		got, ok := fresh.Get(id)
		if !ok {
			t.Fatalf("%s lost on replay", id[:8])
		}
		if got.AuthoritySource != want.AuthoritySource || got.AuthorityActor != want.AuthorityActor ||
			got.AuthorityAt.UnixNano() != want.AuthorityAt.UnixNano() {
			t.Errorf("%s: replayed %v/%x/%d, wrote %v/%x/%d", id[:8],
				got.AuthoritySource, got.AuthorityActor.Id[:2], got.AuthorityAt.UnixNano(),
				want.AuthoritySource, want.AuthorityActor.Id[:2], want.AuthorityAt.UnixNano())
		}
	}
	old, ok := fresh.Get("0123456789abcdef0123456789abcdef")
	if !ok {
		t.Fatal("legacy record lost")
	}
	if old.AuthoritySource != protocol.AuthoritySource_Unknown || old.AuthorityActor != (protocol.TaskID{}) || !old.AuthorityAt.IsZero() {
		t.Errorf("legacy record = %v/%x/%v, want unknown, zero, zero", old.AuthoritySource, old.AuthorityActor.Id[:2], old.AuthorityAt)
	}
}
