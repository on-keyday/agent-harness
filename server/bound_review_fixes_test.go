package server

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A key unenrolled between the gate's check and registration has no record.
// Read as "no binding" that would register the runner UNBOUND — the widest
// answer — so registration is refused instead (bound enrollment, C2).
func TestRegistrationRefusedWhenKeyRecordIsGone(t *testing.T) {
	reg := NewRegistry()
	h := newTestRunnerHandler(reg)
	key := dialTestKey(8)
	h.AuthOf = func(objproto.ConnectionID) ed25519.PublicKey { return key }
	h.BindingOf = func(ed25519.PublicKey) ([]protocol.TaskID, bool) { return nil, false }
	var kicked []objproto.ConnectionID
	h.KickConn = func(c objproto.ConnectionID) { kicked = append(kicked, c) }
	cid := pcid(t, "ws:127.0.0.1:9741-1")
	msg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	msg.SetHello(protocol.RunnerHello{Version: 1, RunnerId: runnerIDOf("gone-key"), MaxTasks: 1})
	h.Handle(&fakeConn{id: cid}, msg.MustAppend(nil))
	if _, ok := reg.Get(cid); ok {
		t.Fatal("a runner whose key record is gone registered (as unbound)")
	}
	if len(kicked) != 1 || kicked[0] != cid {
		t.Fatalf("kicked %v, want the refused connection", kicked)
	}
}

// A schedule bound to an existing task fires as a resume of that task, which
// a runner bound to it may take: creation must not be refused no_candidate
// when that runner is the only one serving the repo.
func TestScheduleOnBoundTaskAcceptsItsBoundRunner(t *testing.T) {
	e := newSchedEnv(t)
	taskHex := e.h.Tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead}, "claude")
	e.h.Tasks.Assign(taskHex, testRunnerID("RB"), "/wt", false)
	e.h.Tasks.Finish(taskHex, 0, nil)
	now := time.Now()
	e.h.Registry.Add(&RunnerEntry{ID: tcid("RB"), Identity: testRunnerID("RB"), Hostname: "h", AllowedRoots: []string{"/r"},
		AgentProfiles: []string{"claude"}, MaxTasks: 2, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now,
		Conn: &fakeConn{}, Bound: boundTo(taskHex)})
	r := e.create(t, schedOpConn().ConnectionID(), bindSpec(t, createSpec(), taskHex), "go")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("schedule on task %s with only its bound runner: %v %q, want ok", taskHex[:8], r.Status, r.Message)
	}
}
