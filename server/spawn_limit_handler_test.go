package server

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// limitFixture is a handler with one runner on /r and an agent P (created by
// the operator, holding delegHeld) carrying the given limits, plus n children
// of P already in the store. It returns P, its connection id, the runner's
// stream and the children.
func limitFixture(t *testing.T, depth, max protocol.SpawnLimit, n int) (h *TaskHandler, p, cid string, runner *fakeBidiStream, kids []string) {
	t.Helper()
	h = newTestHandler(t)
	runner = openRunner(t, h)
	p = h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: delegHeld, SpawnDepth: depth, MaxDescendants: max}, "")
	for i := 0; i < n; i++ {
		k := h.Tasks.Create("/r", "k", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, hexToTaskID(t, p), "",
			protocol.RunnerSelector{}, nil, authority.Grant{}, "")
		h.Tasks.Assign(k, testRunnerID("R"), "/wt/k", false)
		h.Tasks.Finish(k, 0, nil)
		kids = append(kids, k)
	}
	return h, p, bindPrincipal(t, h, p), runner, kids
}

const ownFullText = "your own descendant limit is reached (1/1); resume an existing task, or ask the operator to prune or raise the limit"

func TestSubmitRefusedBySpawnLimit(t *testing.T) {
	h, _, cid, _, _ := limitFixture(t, authority.Unbounded(), authority.LimitOf(1), 1)
	before := len(h.Tasks.List(0))
	resp, _ := h.handleSubmit(cid, submitReq(delegReq{}), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
	if resp.Status != protocol.SubmitStatus_SpawnLimit || string(resp.ErrorMsg) != ownFullText {
		t.Fatalf("status %v msg %q, want spawn_limit %q", resp.Status, resp.ErrorMsg, ownFullText)
	}
	if len(h.Tasks.List(0)) != before {
		t.Error("a refused submit added a row")
	}
}

// Review Focus 4: a refused open leaves no task row and sends the runner
// nothing.
func TestOpenInteractiveRefusedLeavesNoTask(t *testing.T) {
	h, _, cid, runner, _ := limitFixture(t, authority.Unbounded(), authority.LimitOf(1), 1)
	before := len(h.Tasks.List(0))
	resp, _ := h.handleOpenInteractive(cid, nil, detachedOpen(0, 0), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
	if resp.Status != protocol.OpenInteractiveStatus_SpawnLimit || string(resp.ErrorMsg) != ownFullText {
		t.Fatalf("status %v msg %q, want spawn_limit %q", resp.Status, resp.ErrorMsg, ownFullText)
	}
	if len(h.Tasks.List(0)) != before {
		t.Error("a refused open added a row")
	}
	runner.writeMu.Lock()
	n := len(runner.written)
	runner.writeMu.Unlock()
	if n != 0 {
		t.Errorf("the runner was sent %d bytes for a refused open", n)
	}
}

func TestSpawnWritesChildLimits(t *testing.T) {
	h, _, cid, _, _ := limitFixture(t, authority.LimitOf(3), authority.Unbounded(), 0)
	req := submitReq(delegReq{})
	req.SpawnDepth, req.MaxDescendants = authority.LimitOf(5), authority.LimitOf(4)
	resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("submit: %v %s", resp.Status, resp.ErrorMsg)
	}
	e := entryOf(t, h, resp.TaskId)
	if !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(2)) || !sameSpawnLimit(e.MaxDescendants, authority.LimitOf(4)) {
		t.Errorf("requested 5/4 under depth 3: got %s/%s, want 2/4",
			authority.LimitLabel(e.SpawnDepth), authority.LimitLabel(e.MaxDescendants))
	}
	oresp, _ := h.handleOpenInteractive(cid, nil, detachedOpen(0, 0), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
	if oresp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("open: %v %s", oresp.Status, oresp.ErrorMsg)
	}
	e = entryOf(t, h, oresp.TaskId)
	if !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(2)) || e.MaxDescendants.Limited() {
		t.Errorf("omitted under depth 3: got %s/%s, want 2/unbounded",
			authority.LimitLabel(e.SpawnDepth), authority.LimitLabel(e.MaxDescendants))
	}
}

// D9: a resume adds no record and is not checked; a re-granting resume
// writes the depth as a creation would.
func TestResumeNotCheckedAndRegrants(t *testing.T) {
	h, p, cid, _, kids := limitFixture(t, authority.LimitOf(3), authority.LimitOf(1), 1)
	req := submitReq(delegReq{})
	req.ResumeTaskId = hexToTaskID(t, kids[0])
	req.SpawnDepth = authority.LimitOf(5)
	req.SetSpawnDepthPresent(true)
	resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("resume under a full parent: %v %s", resp.Status, resp.ErrorMsg)
	}
	if n := h.Tasks.DescendantCounts()[p]; n != 1 {
		t.Errorf("a resume changed the count to %d", n)
	}
	if e := entryOf(t, h, resp.TaskId); !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(2)) {
		t.Errorf("re-granted depth = %s, want 2", authority.LimitLabel(e.SpawnDepth))
	}
}

func TestExactResumeRefusesLimitBits(t *testing.T) {
	for _, c := range []struct {
		flag string
		set  func(*protocol.SubmitRequest)
	}{
		{"--spawn-depth", func(r *protocol.SubmitRequest) { r.SetSpawnDepthPresent(true) }},
		{"--max-descendants", func(r *protocol.SubmitRequest) { r.SetMaxDescendantsPresent(true) }},
	} {
		h, _, cid, _, kids := limitFixture(t, authority.Unbounded(), authority.Unbounded(), 1)
		req := &protocol.SubmitRequest{ResumeTaskId: hexToTaskID(t, kids[0])}
		req.SetResumeExact(true)
		c.set(req)
		resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
		if resp.Status != protocol.SubmitStatus_ResumeExactMismatch || !strings.Contains(string(resp.ErrorMsg), c.flag) {
			t.Errorf("%s: status %v msg %q, want resume_exact_mismatch naming it", c.flag, resp.Status, resp.ErrorMsg)
		}
	}
}

func TestListCarriesDescendants(t *testing.T) {
	h, p, _, _, _ := limitFixture(t, authority.Unbounded(), authority.LimitOf(5), 2)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9970-1"), nextSendStreamID: 10}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_List}}
	req.SetList(protocol.ListQuery{})
	h.Handle(conn, encodeTaskControlRequest(t, req))
	if len(conn.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(conn.sendStreams))
	}
	body := decodeListBody(t, conn.sendStreams[0].bytes)
	for _, ti := range body.Tasks {
		if hex.EncodeToString(ti.Id.Id[:]) != p {
			continue
		}
		if ti.Descendants != 2 || !sameSpawnLimit(ti.MaxDescendants, authority.LimitOf(5)) {
			t.Errorf("P's row: descendants %d max %s, want 2 and 5", ti.Descendants, authority.LimitLabel(ti.MaxDescendants))
		}
		return
	}
	t.Fatal("P not listed")
}

// An agent resuming a task re-grants its descendant limit only downward: the
// limit it would otherwise widen may be the very one bounding it. A is ended
// and holds max_descendants=1; its child R (spawn, resume, scope naming A) is
// refused a spawn, resumes A asking for unbounded, and must still be refused.
func TestResumeRegrantCannotWidenAncestorLimit(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	a := h.Tasks.Create("/r", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{MaxDescendants: authority.LimitOf(1)}, "")
	h.Tasks.Assign(a, testRunnerID("R"), "/wt/a", false)
	h.Tasks.Finish(a, 0, nil)
	r := h.Tasks.Create("/r", "r", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, hexToTaskID(t, a), "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: delegHeld, Scope: authority.Scope{IDs: []string{a}}}, "")
	cid := bindPrincipal(t, h, r)

	if resp, _ := h.handleSubmit(cid, submitReq(delegReq{}), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld); resp.Status != protocol.SubmitStatus_SpawnLimit {
		t.Fatalf("spawn under a full A: %v, want spawn_limit", resp.Status)
	}
	req := submitReq(delegReq{})
	req.ResumeTaskId = hexToTaskID(t, a)
	req.SetMaxDescendantsPresent(true) // the zero value: unbounded
	if resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld); resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("resume of A: %v %s", resp.Status, resp.ErrorMsg)
	}
	if e, _ := h.Tasks.Get(a); !sameSpawnLimit(e.MaxDescendants, authority.LimitOf(1)) {
		t.Errorf("an agent's resume widened A's limit to %s", authority.LimitLabel(e.MaxDescendants))
	}
	if resp, _ := h.handleSubmit(cid, submitReq(delegReq{}), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld); resp.Status != protocol.SubmitStatus_SpawnLimit {
		t.Errorf("spawn after the resume: %v, want spawn_limit", resp.Status)
	}
}

// A creation the limits refuse is refused before runner resolution: a full
// or depth-0 agent hears spawn_limit, not no_runner or a runner picker.
func TestSpawnLimitBeforeRunnerResolution(t *testing.T) {
	h := newTestHandler(t) // no runner registered
	p := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: delegHeld, SpawnDepth: authority.LimitOf(0)}, "")
	cid := bindPrincipal(t, h, p)
	if resp, _ := h.handleSubmit(cid, submitReq(delegReq{}), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld); resp.Status != protocol.SubmitStatus_SpawnLimit {
		t.Errorf("submit: %v, want spawn_limit", resp.Status)
	}
	if resp, _ := h.handleOpenInteractive(cid, nil, detachedOpen(0, 0), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld); resp.Status != protocol.OpenInteractiveStatus_SpawnLimit {
		t.Errorf("open: %v, want spawn_limit", resp.Status)
	}
}

// Through the handler, generation by generation: operator→A(3)→B→C→D, and D
// may create nothing.
func TestHandlerDepthChain(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	cur := h.Tasks.Create("/r", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: delegHeld, DelegateCaps: delegHeld, SpawnDepth: authority.LimitOf(3)}, "")
	for gen, want := range []uint32{2, 1, 0} {
		cid := bindPrincipal(t, h, cur)
		req := submitReq(delegReq{caps: delegHeld, dcaps: delegHeld})
		resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
		if resp.Status != protocol.SubmitStatus_Ok {
			t.Fatalf("generation %d: %v %s", gen+1, resp.Status, resp.ErrorMsg)
		}
		e := entryOf(t, h, resp.TaskId)
		if !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(want)) {
			t.Fatalf("generation %d depth %s, want %d", gen+1, authority.LimitLabel(e.SpawnDepth), want)
		}
		cur = e.ID
	}
	cid := bindPrincipal(t, h, cur)
	if resp, _ := h.handleSubmit(cid, submitReq(delegReq{}), protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld); resp.Status != protocol.SubmitStatus_SpawnLimit {
		t.Errorf("D at depth 0: %v, want spawn_limit", resp.Status)
	}
}
