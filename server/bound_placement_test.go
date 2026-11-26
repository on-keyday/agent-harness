package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// C2 of 2026-11-26-grant-conditions-and-bound-enrollment-design.md: a runner
// that authenticated with a key bound to S is a candidate only for tasks in S.

func boundTo(ids ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

const (
	taskT = "000000000000000000000000000000a1"
	taskU = "000000000000000000000000000000b2"
)

func candidateIDs(cs []RunnerEntry) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID.String())
	}
	return out
}

func TestCandidatesSkipsBoundRunnerForOtherTask(t *testing.T) {
	reg := NewRegistry()
	r := idleRunner("bound", "h", nil)
	r.Bound = boundTo(taskT)
	reg.Add(r)
	if got := reg.Candidates("/x", protocol.RunnerSelector{}, taskU); len(got) != 0 {
		t.Fatalf("candidates for U = %v, want none: the runner is bound to T", candidateIDs(got))
	}
}

func TestCandidatesBoundRunnerTakesItsTask(t *testing.T) {
	reg := NewRegistry()
	r := idleRunner("bound", "h", nil)
	r.Bound = boundTo(taskT)
	reg.Add(r)
	if got := reg.Candidates("/x", protocol.RunnerSelector{}, taskT); len(got) != 1 {
		t.Fatalf("candidates for T = %v, want the bound runner", candidateIDs(got))
	}
}

// "" is "no task yet" (a new foreground open, a schedule check at creation):
// it matches no binding, and an unbound runner as before.
func TestCandidatesNoTaskMatchesNoBinding(t *testing.T) {
	reg := NewRegistry()
	b := idleRunner("bound", "h", nil)
	b.Bound = boundTo(taskT)
	reg.Add(b)
	reg.Add(idleRunner("free", "h", nil))
	got := reg.Candidates("/x", protocol.RunnerSelector{}, "")
	if len(got) != 1 || got[0].ID != tcid("free") {
		t.Fatalf("candidates for no task = %v, want only the unbound runner", candidateIDs(got))
	}
}

// With an unbound and a bound runner on one repo, U goes to the unbound one
// and never to the runner bound to T.
func TestTickPlacesOtherTaskOnUnboundRunner(t *testing.T) {
	reg := NewRegistry()
	free := idleRunner("free", "h", nil)
	free.MaxTasks = 1
	reg.Add(free)
	store := NewTaskStore()
	u := queueBG(t, store, "/x", "U", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
	tt := queueBG(t, store, "/x", "T", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
	bound := idleRunner("bound", "h", nil)
	bound.Bound = boundTo(tt)
	bound.ConnectedAt = time.Unix(0, 0) // first in Candidates' order
	reg.Add(bound)

	var captured []assignPair
	s := NewScheduler(reg, store, func(runnerID objproto.ConnectionID, tID string) error {
		captured = append(captured, assignPair{runnerID, tID})
		return nil
	})
	s.Tick()
	want := map[string]objproto.ConnectionID{u: tcid("free"), tt: tcid("bound")}
	if len(captured) != 2 {
		t.Fatalf("assignments %v, want U→free and T→bound", captured)
	}
	for _, p := range captured {
		if want[p.task] != p.runner {
			t.Errorf("task %s went to %v, want %v", p.task[:8], p.runner, want[p.task])
		}
	}
}

// A new foreground open has no task id yet: a bound runner is not a candidate,
// so with only a bound runner serving the repo the open answers
// no_runner_for_repo rather than placing on it or offering it to pick.
func TestForegroundOpenSkipsBoundRunner(t *testing.T) {
	open := func(bound map[string]struct{}) protocol.OpenInteractiveStatus {
		h := newTestHandler(t)
		now := time.Now()
		h.Registry.Add(&RunnerEntry{ID: tcid("B"), Identity: testRunnerID("B"), Hostname: "h", AllowedRoots: []string{"/x/repo"},
			MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: stubConn{}, Bound: bound})
		req := &protocol.OpenInteractiveRequest{}
		req.SetRepoPath([]byte("/x/repo"))
		resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All, "")
		return resp.Status
	}
	if st := open(boundTo(taskT)); st != protocol.OpenInteractiveStatus_NoRunnerForRepo {
		t.Fatalf("only a bound runner: status=%v, want no_runner_for_repo", st)
	}
	if st := open(nil); st == protocol.OpenInteractiveStatus_NoRunnerForRepo {
		t.Fatal("control: an unbound runner must be a candidate for the same open")
	}
}

// The binding is read from the key store when the runner registers.
func TestRegistrationReadsBinding(t *testing.T) {
	reg := NewRegistry()
	h := newTestRunnerHandler(reg)
	key := dialTestKey(7)
	h.AuthOf = func(objproto.ConnectionID) ed25519.PublicKey { return key }
	tid := hexToTaskID(t, taskT)
	h.BindingOf = func(k ed25519.PublicKey) ([]protocol.TaskID, bool) {
		if k.Equal(key) {
			return []protocol.TaskID{tid}, true
		}
		return nil, false
	}
	h.KickConn = func(objproto.ConnectionID) {}
	cid := pcid(t, "ws:127.0.0.1:9740-1")
	msg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	msg.SetHello(protocol.RunnerHello{Version: 1, RunnerId: runnerIDOf("bound-reg"), MaxTasks: 1})
	h.Handle(&fakeConn{id: cid}, msg.MustAppend(nil))
	e, ok := reg.Get(cid)
	if !ok {
		t.Fatal("the runner did not register")
	}
	if _, has := e.Bound[taskT]; !has || len(e.Bound) != 1 {
		t.Fatalf("RunnerEntry.Bound = %v, want {T}", e.Bound)
	}
}

// Readopt places held tasks without Candidates, so it applies the binding
// itself: a held task the reporting runner's binding does not name is refused.
func TestReadoptRefusesTaskOutsideBinding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bound    func(taskID string) map[string]struct{}
		accepted int
	}{
		{"binding names it", func(id string) map[string]struct{} { return boundTo(id) }, 1},
		{"binding names another task", func(string) map[string]struct{} { return boundTo(taskU) }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, reg, store, identity, cid, taskID := readoptFixture(t)
			e, _ := reg.Get(cid)
			e.Bound = tc.bound(taskID)
			reg.Add(&e)
			res := srv.readoptHeldTasks(identity, heldReportFor(t, store, identity, taskID))
			if len(res.Accepted) != tc.accepted {
				t.Fatalf("accepted %d, want %d", len(res.Accepted), tc.accepted)
			}
		})
	}
}

// RunnerInfo carries the binding the registry entry holds.
func TestToRunnerInfoCarriesBinding(t *testing.T) {
	e := RunnerEntry{Bound: boundTo(taskT), Conn: &fakeConn{}}
	info := toRunnerInfo(e)
	if len(info.Bound) != 1 || !strings.EqualFold(hex.EncodeToString(info.Bound[0].Id[:]), taskT) {
		t.Fatalf("RunnerInfo.Bound = %v, want [T]", info.Bound)
	}
}
