package server

import (
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

type kickFixture struct {
	*dialerFixture
	h      *TaskHandler
	mu     sync.Mutex
	kicked []objproto.ConnectionID
}

func newKickFixture(t *testing.T) *kickFixture {
	t.Helper()
	df := newDialerFixture(t)
	f := &kickFixture{dialerFixture: df}
	f.h = newTestTaskHandler(NewTaskStore())
	f.h.Registry = df.n.reg
	f.h.RunnerDials = df.store
	f.h.Dials = df.n.m
	f.h.KickWait = 100 * time.Millisecond
	f.h.KickConn = func(cid objproto.ConnectionID) {
		f.mu.Lock()
		f.kicked = append(f.kicked, cid)
		f.mu.Unlock()
	}
	df.n.m.Start(df.ctx)
	return f
}

// addRunner registers a runner directly, as an accepted connection would.
func (f *kickFixture) addRunner(t *testing.T, cid string, identity string, tasks ...string) RunnerEntry {
	t.Helper()
	id := pcid(t, cid)
	active := map[string]struct{}{}
	for _, task := range tasks {
		active[task] = struct{}{}
	}
	e := &RunnerEntry{ID: id, Identity: runnerIDOf(identity), Hostname: "h-" + identity, Key: f.key,
		Conn: &fakeConn{id: id}, ActiveTasks: active, MaxTasks: 4}
	if _, err := f.n.reg.Add(e); err != nil {
		t.Fatal(err)
	}
	got, _ := f.n.reg.Get(id)
	return got
}

func (f *kickFixture) kick(t *testing.T, conn *fakeConn, prefix string, force bool) (protocol.RunnerKickStatus, *protocol.RunnerKickBody) {
	t.Helper()
	r := protocol.RunnerKickRequest{}
	r.SetPrefix([]byte(prefix))
	r.SetForce(force)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerKick, RequestId: 6}
	req.SetRunnerKick(r)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	var resp protocol.TaskControlResponse
	eventually(t, "a runner_kick answer", func() bool {
		conn.sentMu.Lock()
		n := len(conn.sent)
		conn.sentMu.Unlock()
		if n == 0 {
			return false
		}
		resp = lastTaskControlResponse(t, conn)
		return resp.RunnerKick() != nil
	})
	k := resp.RunnerKick()
	if k.StreamId == 0 {
		return k.Status, nil
	}
	var body protocol.RunnerKickBody
	if err := body.DecodeExact(conn.sendStreamBytes(t, k.StreamId)); err != nil {
		t.Fatal(err)
	}
	return k.Status, &body
}

// leaveSentTo reports the `by` of a leave request sent on c, if any.
func leaveSentTo(c ConnHandle) (string, bool) {
	fc := c.(*fakeConn)
	fc.sentMu.Lock()
	defer fc.sentMu.Unlock()
	for _, b := range fc.sent {
		if len(b) == 0 || appwire.AppKind(b[0]) != appwire.AppKind_RunnerControl {
			continue
		}
		var req protocol.RunnerRequest
		if _, err := req.Decode(b[1:]); err == nil && req.Kind == protocol.RunnerRequestType_Leave {
			return string(req.Leave().By), true
		}
	}
	return "", false
}

func identityHex(s string) string { return runnerIDOf(s).Hex() }

func TestKickStatuses(t *testing.T) {
	f := newKickFixture(t)
	op := keyVerbConn()
	if st, _ := f.kick(t, op, "ffffffff", false); st != protocol.RunnerKickStatus_NotFound {
		t.Fatalf("no such runner: %v", st)
	}
	f.addRunner(t, "ws:127.0.0.1:9600-1", "twin-a")
	f.addRunner(t, "ws:127.0.0.1:9601-1", "twin-b") // shares a long hex prefix with twin-a
	if st, _ := f.kick(t, op, identityHex("twin-a")[:8], false); st != protocol.RunnerKickStatus_AmbiguousPrefix {
		t.Fatalf("ambiguous: %v", st)
	}
	busy := f.addRunner(t, "ws:127.0.0.1:9602-1", "busy", "00112233445566778899aabbccddeeff")
	st, body := f.kick(t, op, busy.Identity.Hex(), false)
	if st != protocol.RunnerKickStatus_InUse || body == nil || len(body.Tasks) != 1 {
		t.Fatalf("in_use: %v %+v", st, body)
	}
	if _, sent := leaveSentTo(busy.Conn); sent {
		t.Fatal("leave was sent to a runner the kick refused")
	}
	if len(f.kicked) != 0 {
		t.Fatal("a refused kick closed a connection")
	}
}

func TestKickNeedsRunnerEnroll(t *testing.T) {
	if got := requiredCap[protocol.TaskControlKind_RunnerKick]; got != protocol.Capability_RunnerEnroll {
		t.Fatalf("runner_kick gated on %v, want runner_enroll", got)
	}
	f := newKickFixture(t)
	e := f.addRunner(t, "ws:127.0.0.1:9603-1", "victim")
	agent := keyVerbConn()
	tid := f.h.Tasks.Create("r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_RunnerAdmin}, "")
	f.h.principals = map[string]protocol.TaskID{agent.ConnectionID().String(): hexToTaskID(t, tid)}
	r := protocol.RunnerKickRequest{}
	r.SetPrefix([]byte(e.Identity.Hex()))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerKick, RequestId: 1}
	req.SetRunnerKick(r)
	f.h.Handle(agent, encodeTaskControlRequest(t, req))
	if resp := lastTaskControlResponse(t, agent); resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("runner_admin alone kicked a runner: %v", resp.Kind)
	}
	if _, sent := leaveSentTo(e.Conn); sent {
		t.Fatal("leave was sent despite the denial")
	}
}

func TestKickLeftWhenRunnerCloses(t *testing.T) {
	f := newKickFixture(t)
	e := f.addRunner(t, "ws:127.0.0.1:9604-1", "leaver")
	go func() { // the runner obeys: it ends its connection
		for {
			if _, ok := leaveSentTo(e.Conn); ok {
				f.n.reg.Remove(e.ID)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	st, body := f.kick(t, keyVerbConn(), e.Identity.Hex()[:12], false)
	if st != protocol.RunnerKickStatus_Left || body == nil || string(body.Runner.Hostname) != "h-leaver" {
		t.Fatalf("left: %v %+v", st, body)
	}
	if by, _ := leaveSentTo(e.Conn); by != "operator" {
		t.Fatalf("leave by = %q, want operator", by)
	}
	if len(f.kicked) != 0 {
		t.Fatal("the server closed a connection the runner closed itself")
	}
}

func TestKickOldRunnerFallsBackToClose(t *testing.T) {
	f := newKickFixture(t)
	e := f.addRunner(t, "ws:127.0.0.1:9605-1", "deaf") // never acts on leave
	st, _ := f.kick(t, keyVerbConn(), e.Identity.Hex(), false)
	if st != protocol.RunnerKickStatus_ClosedWithoutLeaving {
		t.Fatalf("status %v, want closed_without_leaving", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kicked) != 1 || f.kicked[0] != e.ID {
		t.Fatalf("kicked %v, want [%v]", f.kicked, e.ID)
	}
}

func TestKickForceEndsBusyRunner(t *testing.T) {
	f := newKickFixture(t)
	e := f.addRunner(t, "ws:127.0.0.1:9606-1", "busy2", "00112233445566778899aabbccddeeff")
	st, body := f.kick(t, keyVerbConn(), e.Identity.Hex(), true)
	if st != protocol.RunnerKickStatus_ClosedWithoutLeaving || body == nil || len(body.Tasks) != 1 {
		t.Fatalf("forced: %v %+v", st, body)
	}
	if _, sent := leaveSentTo(e.Conn); !sent {
		t.Fatal("no leave sent")
	}
}

func TestKickRemovesLiveRecord(t *testing.T) {
	f := newKickFixture(t)
	rec := f.add(t, "ws:127.0.0.1:9607-*")
	eventually(t, "connected", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	e, _ := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
	_, body := f.kick(t, keyVerbConn(), e.Identity.Hex(), false)
	if body == nil || len(body.RemovedDials) != 1 || body.RemovedDials[0] != rec.ID {
		t.Fatalf("removed_dials %+v, want [%d]", body, rec.ID)
	}
	if _, ok := f.store.Get(rec.ID); ok || f.n.m.running(rec.ID) {
		t.Fatal("the record survived the kick")
	}
}

func TestKickRemovesRecordByAddressAfterRunnerRestart(t *testing.T) {
	f := newKickFixture(t)
	f.n.fail = func(objproto.ConnectionID) error { return errTestRefused }
	rec := f.add(t, "ws:127.0.0.1:9608-*") // never connects: waiting
	eventually(t, "waiting", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Waiting })
	// The restarted runner is connected some other way at the record's address.
	e := f.addRunner(t, "ws:127.0.0.1:9608-3", "restarted")
	_, body := f.kick(t, keyVerbConn(), e.Identity.Hex(), false)
	if body == nil || len(body.RemovedDials) != 1 || body.RemovedDials[0] != rec.ID {
		t.Fatalf("removed_dials %+v, want [%d]", body, rec.ID)
	}
}

var errTestRefused = errorString("refused")

type errorString string

func (e errorString) Error() string { return string(e) }
