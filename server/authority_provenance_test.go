package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
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
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}},
		"")

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
	s.SetCaps(id, GrantUpdate{Caps: true, Scope: false, To: authority.Grant{Caps: protocol.Capability_Cancel, Scope: authority.Scope{}}},
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
	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Agent, GrantUpdate{Caps: true, Scope: false, To: authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.Scope{}}},
		protocol.TaskKind_Oneshot, "", resumer); err != nil {
		t.Fatalf("resume: %v", err)
	}
	wantProvenance(t, s, id, protocol.AuthoritySource_Resume, resumer)

	// A resume that keeps the recorded authority writes nothing.
	kept, _ := s.Get(id)
	s.Finish(id, 0, nil)
	if _, err := s.Resume(id, "p3", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, GrantUpdate{Caps: false, Scope: false, To: authority.Grant{Caps: 0, Scope: authority.Scope{}}},
		protocol.TaskKind_Oneshot, "", protocol.TaskID{}); err != nil {
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
	s.SetCaps(c, GrantUpdate{Caps: true, Scope: false, To: authority.Grant{Caps: protocol.Capability_Cancel, Scope: authority.Scope{}}},
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

// The operator's caps set writes operator_caps_set on its target, and the
// cascade it drives writes cascade, with the target as actor, on each
// descendant it clamped.
func TestSetCapsWritesProvenance(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9930-1")}
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}.ToWire()}
	req.SetScopePresent(true)
	req.SetCascade(true)
	if got := sendSetCaps(t, h, opConn, req); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	wantProvenance(t, h.Tasks, c, protocol.AuthoritySource_OperatorCapsSet, protocol.TaskID{})
	wantProvenance(t, h.Tasks, g, protocol.AuthoritySource_Cascade, hexToTaskID(t, c))
}

// A resume that re-grants records its resumer: the agent's task, or nobody
// for an operator.
func TestResumeWritesResumer(t *testing.T) {
	f := newResumeFixture(t, protocol.Capability_Resume|protocol.Capability_FileRead, authority.Scope{})
	now := time.Now()
	f.h.Registry.Add(&RunnerEntry{ID: tcid("R"), Hostname: "r", AllowedRoots: []string{"/r"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: stubConn{}})
	resumeWith := func(conn *fakeConn, id string) {
		sub := protocol.SubmitRequest{ResumeTaskId: hexToTaskID(t, id), RequestedCaps: protocol.Capability_FileRead}
		sub.SetRepoPath([]byte("/r"))
		sub.SetPrompt([]byte("again"))
		sub.SetResumeCapsOverride(true)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit, RequestId: 9}
		req.SetSubmit(sub)
		f.h.Handle(conn, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, conn)
		if s := resp.Submit(); s == nil || s.Status != protocol.SubmitStatus_Ok {
			t.Fatalf("resume of %s: %v", id[:8], s)
		}
	}
	byAgent := f.terminalTask(t, true)
	resumeWith(f.conn, byAgent)
	wantProvenance(t, f.h.Tasks, byAgent, protocol.AuthoritySource_Resume, hexToTaskID(t, f.selfID))

	byOperator := f.terminalTask(t, true)
	resumeWith(&fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9931-1")}, byOperator)
	wantProvenance(t, f.h.Tasks, byOperator, protocol.AuthoritySource_Resume, protocol.TaskID{})
}

// A caller is not told the id of an actor it cannot see. Its one-hop parent
// counts as seen: that id is already on its redacted parent row.
func TestListHidesUnseenActor(t *testing.T) {
	h, p, c, g, _ := scopeFixture(t)
	h.Registry = NewRegistry() // the operator's list reads the runners
	setScope(t, h, g, authority.Scope{Base: protocol.ScopeBase_None})
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9932-1")}
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, p), Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}.ToWire()}
	req.SetScopePresent(true)
	req.SetCascade(true)
	if got := sendSetCaps(t, h, opConn, req); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	conn := func(port int, task string) *fakeConn {
		fc := &fakeConn{id: objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%d-1", port)), nextSendStreamID: 12}
		h.principals[fc.ConnectionID().String()] = hexToTaskID(t, task)
		return fc
	}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}

	row, ok := listRowAs(t, h, conn(9933, g), g)
	if !ok {
		t.Fatal("g does not list itself")
	}
	if row.AuthorityActor != (protocol.TaskID{}) || !row.AuthorityActorHidden() {
		t.Errorf("g sees its actor as %x hidden=%v, want withheld: p is its grandparent", row.AuthorityActor.Id[:2], row.AuthorityActorHidden())
	}
	if row.AuthoritySource != protocol.AuthoritySource_Cascade || row.AuthorityAt == 0 {
		t.Errorf("g's source/time = %v/%d, want them shown even when the actor is not", row.AuthoritySource, row.AuthorityAt)
	}

	row, _ = listRowAs(t, h, conn(9934, c), g)
	if row.AuthorityActor != hexToTaskID(t, p) || row.AuthorityActorHidden() {
		t.Errorf("c sees g's actor as %x hidden=%v, want p: p is c's one-hop parent", row.AuthorityActor.Id[:2], row.AuthorityActorHidden())
	}

	row, _ = listRowAs(t, h, &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9935-1"), nextSendStreamID: 12}, g)
	if row.AuthorityActor != hexToTaskID(t, p) || row.AuthorityActorHidden() {
		t.Errorf("operator sees g's actor as %x hidden=%v, want p", row.AuthorityActor.Id[:2], row.AuthorityActorHidden())
	}
}

// The age is computed on the server; a task with no record reports neither.
func TestListAuthorityAge(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	h.Registry = NewRegistry()
	h.Tasks.SetCaps(c, GrantUpdate{Caps: true, Scope: false, To: authority.Grant{Caps: protocol.Capability_Cancel, Scope: authority.Scope{}}},
		AuthorityWrite{Source: protocol.AuthoritySource_OperatorCapsSet})

	h.Tasks.tasks[c].AuthorityAt = time.Now().Add(-2 * time.Second)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9936-1"), nextSendStreamID: 12}
	row, _ := listRowAs(t, h, opConn, c)
	if row.AuthorityAt == 0 || row.AuthorityAgeMs < 1000 || row.AuthorityAgeMs > 60000 {
		t.Errorf("at=%d age=%dms, want a 2s-old write", row.AuthorityAt, row.AuthorityAgeMs)
	}
	h.Tasks.tasks[c].AuthorityAt = time.Time{}
	h.Tasks.tasks[c].AuthoritySource = protocol.AuthoritySource_Unknown
	row, _ = listRowAs(t, h, &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9937-1"), nextSendStreamID: 12}, c)
	if row.AuthorityAt != 0 || row.AuthorityAgeMs != 0 {
		t.Errorf("unknown row at=%d age=%d, want 0 and 0", row.AuthorityAt, row.AuthorityAgeMs)
	}
}
