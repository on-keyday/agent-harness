package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// resumeFixture is a handler with one principal task bound to conn.
type resumeFixture struct {
	h      *TaskHandler
	conn   *fakeConn
	selfID string
	reqID  uint32
}

func newResumeFixture(t *testing.T, caps protocol.Capability, scope authority.Scope) *resumeFixture {
	t.Helper()
	h := newTestHandler(t)
	self := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, caps, scope, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9710-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, self)
	return &resumeFixture{h: h, conn: conn, selfID: self}
}

// terminalTask creates a finished task; creator is the principal when child is set.
func (f *resumeFixture) terminalTask(t *testing.T, child bool) string {
	t.Helper()
	var creator protocol.TaskID
	if child {
		creator = hexToTaskID(t, f.selfID)
	}
	id := f.h.Tasks.Create("/r", "old", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		creator, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, authority.Scope{}, "")
	markTerminalForTest(t, f.h, id)
	return id
}

// submit sends a SubmitRequest; resume == "" is a fresh spawn.
func (f *resumeFixture) submit(t *testing.T, resume string, exact bool) protocol.TaskControlResponse {
	t.Helper()
	f.reqID++
	sub := protocol.SubmitRequest{}
	sub.SetRepoPath([]byte("/r"))
	if resume == "" {
		sub.SetPrompt([]byte("p"))
	} else {
		sub.ResumeTaskId = hexToTaskID(t, resume)
	}
	sub.SetResumeExact(exact)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit, RequestId: f.reqID}
	req.SetSubmit(sub)
	f.h.Handle(f.conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, f.conn)
}

// open sends a detached OpenInteractiveRequest, so the seat never decides it.
func (f *resumeFixture) open(t *testing.T, resume string, exact bool) protocol.TaskControlResponse {
	t.Helper()
	f.reqID++
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	if resume != "" {
		oi.ResumeTaskId = hexToTaskID(t, resume)
	}
	oi.SetResumeExact(exact)
	oi.SetDetached(true)
	oi.SetInitialRows(0) // the size block exists under detached; 0 = no size
	oi.SetInitialCols(0)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: f.reqID}
	req.SetOpenInteractive(oi)
	f.h.Handle(f.conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, f.conn)
}

// resumeStatus reports whether a resume reached past the target gate.
func resumeNotFound(resp protocol.TaskControlResponse) bool {
	if s := resp.Submit(); s != nil {
		return s.Status == protocol.SubmitStatus_ResumeNotFound
	}
	if o := resp.OpenInteractive(); o != nil {
		return o.Status == protocol.OpenInteractiveStatus_ResumeNotFound
	}
	return false
}

// TestResumeGateMatrix: spawn creates and nothing else; resume implies
// resume_exact; neither implies spawn (spec D20).
func TestResumeGateMatrix(t *testing.T) {
	const (
		fresh = "fresh"
		plain = "resume"
		exact = "exact"
	)
	want := map[string]protocol.Capability{
		fresh: protocol.Capability_Spawn,
		plain: protocol.Capability_Resume,
		exact: protocol.Capability_Resume | protocol.Capability_ResumeExact,
	}
	allowed := map[protocol.Capability]map[string]bool{
		protocol.Capability_None:        {},
		protocol.Capability_Spawn:       {fresh: true},
		protocol.Capability_ResumeExact: {exact: true},
		protocol.Capability_Resume:      {plain: true, exact: true},
	}
	for held, ok := range allowed {
		for _, op := range []string{fresh, plain, exact} {
			for _, path := range []string{"submit", "open"} {
				name := fmt.Sprintf("%s/%s/%s", held, op, path)
				t.Run(name, func(t *testing.T) {
					f := newResumeFixture(t, held, authority.Scope{Base: protocol.ScopeBase_Global})
					target := ""
					if op != fresh {
						target = f.terminalTask(t, true)
					}
					send := f.submit
					if path == "open" {
						send = f.open
					}
					resp := send(t, target, op == exact)
					denied := resp.Kind == protocol.TaskControlKind_PermissionDenied
					if denied == ok[op] {
						t.Fatalf("denied=%v, want %v", denied, !ok[op])
					}
					if denied && resp.PermissionDenied().RequiredCap != want[op] {
						t.Fatalf("RequiredCap = %s, want %s",
							authority.CapsLabel(resp.PermissionDenied().RequiredCap), authority.CapsLabel(want[op]))
					}
				})
			}
		}
	}
}

// TestResumeTargetRuleD19: a resume target is allowed when some HELD bit that
// permits this resume has it in that bit's own scope.
func TestResumeTargetRuleD19(t *testing.T) {
	t.Run("resume_override_narrows_plain_resume", func(t *testing.T) {
		f := newResumeFixture(t, protocol.Capability_Resume, authority.Scope{Base: protocol.ScopeBase_Subtree})
		a := f.terminalTask(t, false)
		c := f.terminalTask(t, true)
		f.h.Tasks.mu.Lock()
		f.h.Tasks.tasks[f.selfID].Scope.Overrides = []authority.ScopeOverride{
			{Caps: protocol.Capability_Resume, Base: protocol.ScopeBase_None, IDs: []string{a}},
		}
		f.h.Tasks.mu.Unlock()
		if resumeNotFound(f.submit(t, a, false)) {
			t.Fatal("A is named by the resume override but was refused")
		}
		if !resumeNotFound(f.submit(t, c, false)) {
			t.Fatal("child C is outside the narrowed resume scope but was allowed")
		}
	})
	t.Run("resume_exact_override", func(t *testing.T) {
		f := newResumeFixture(t, protocol.Capability_ResumeExact, authority.Scope{Base: protocol.ScopeBase_None})
		x := f.terminalTask(t, false)
		other := f.terminalTask(t, false)
		f.h.Tasks.mu.Lock()
		f.h.Tasks.tasks[f.selfID].Scope.Overrides = []authority.ScopeOverride{
			{Caps: protocol.Capability_ResumeExact, Base: protocol.ScopeBase_None, IDs: []string{x}},
		}
		f.h.Tasks.mu.Unlock()
		if resumeNotFound(f.submit(t, x, true)) {
			t.Fatal("X refused")
		}
		if !resumeNotFound(f.submit(t, other, true)) {
			t.Fatal("a task outside resume_exact's scope was allowed")
		}
	})
	t.Run("two_bits_two_scopes", func(t *testing.T) {
		f := newResumeFixture(t, protocol.Capability_Resume|protocol.Capability_ResumeExact,
			authority.Scope{Base: protocol.ScopeBase_None})
		a := f.terminalTask(t, false)
		b := f.terminalTask(t, false)
		f.h.Tasks.mu.Lock()
		f.h.Tasks.tasks[f.selfID].Scope.Overrides = []authority.ScopeOverride{
			{Caps: protocol.Capability_Resume, Base: protocol.ScopeBase_None, IDs: []string{a}},
			{Caps: protocol.Capability_ResumeExact, Base: protocol.ScopeBase_None, IDs: []string{b}},
		}
		f.h.Tasks.mu.Unlock()
		if resumeNotFound(f.submit(t, a, true)) || resumeNotFound(f.submit(t, b, true)) {
			t.Fatal("exact resume of A or B refused")
		}
		if resumeNotFound(f.submit(t, a, false)) {
			t.Fatal("plain resume of A refused")
		}
		if !resumeNotFound(f.submit(t, b, false)) {
			t.Fatal("plain resume of B allowed, but only resume_exact's scope names B")
		}
	})
}

// TestResumeOutOfBothScopesIsNotFound: an out-of-scope target answers as
// absent, never as a capability denial — a denial would be an existence oracle.
func TestResumeOutOfBothScopesIsNotFound(t *testing.T) {
	f := newResumeFixture(t, protocol.Capability_ResumeExact, authority.Scope{Base: protocol.ScopeBase_None})
	target := f.terminalTask(t, false)
	for _, send := range []func(*testing.T, string, bool) protocol.TaskControlResponse{f.submit, f.open} {
		resp := send(t, target, true)
		if resp.Kind == protocol.TaskControlKind_PermissionDenied {
			t.Fatal("out-of-scope target answered PermissionDenied")
		}
		if !resumeNotFound(resp) {
			t.Fatalf("out-of-scope target not reported as resume_not_found: %+v", resp)
		}
	}
}

// TestScopeOverrideNamingSpawnRefused: spawn names no task, so an override for
// it would bound nothing (spec D21).
func TestScopeOverrideNamingSpawnRefused(t *testing.T) {
	err := authority.Validate(authority.Scope{Overrides: []authority.ScopeOverride{
		{Caps: protocol.Capability_Spawn | protocol.Capability_FileRead, Base: protocol.ScopeBase_None},
	}})
	if err == nil || !strings.Contains(err.Error(), "spawn names no task") {
		t.Fatalf("err = %v, want a refusal naming spawn", err)
	}
}
