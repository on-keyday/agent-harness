package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func TestHasCap(t *testing.T) {
	have := protocol.Capability_Spawn | protocol.Capability_FileRead
	if !hasCap(have, protocol.Capability_Spawn) {
		t.Error("spawn should be present")
	}
	if hasCap(have, protocol.Capability_FileWrite) {
		t.Error("file_write should be absent")
	}
}

func TestIntersectCaps(t *testing.T) {
	parent := protocol.Capability_Spawn | protocol.Capability_FileRead
	req := protocol.Capability_All // inherit-all
	if got := intersectCaps(parent, req); got != parent {
		t.Fatalf("intersect with all = %#x, want parent %#x", got, parent)
	}
	// request more than parent holds → cannot widen.
	if got := intersectCaps(parent, protocol.Capability_FileWrite); got != protocol.Capability_None {
		t.Fatalf("intersect beyond parent = %#x, want none", got)
	}
}

// ---------------------------------------------------------------------------
// Task 3: callerCaps
// ---------------------------------------------------------------------------

// hexToTaskID decodes a 32-hex string returned by TaskStore.Create into a
// protocol.TaskID. Mirrors the inverse of hexTaskIDProto used in production.
func hexToTaskID(t *testing.T, idHex string) protocol.TaskID {
	t.Helper()
	raw, err := hex.DecodeString(idHex)
	if err != nil {
		t.Fatalf("hexToTaskID(%q): %v", idHex, err)
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	return tid
}

func TestCallerCaps(t *testing.T) {
	h := newTestHandler(t)

	// Operator (no principal entry for this connID) → Capability_All.
	if got := h.callerCaps("operator-conn"); got != protocol.Capability_All {
		t.Fatalf("operator caps = %#x, want All (%#x)", got, protocol.Capability_All)
	}

	// Agent principal whose task exists → that task's stored caps.
	parentCaps := protocol.Capability_FileRead | protocol.Capability_Spawn
	agentTaskIDHex := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, parentCaps, Scope{}, "")
	tid := hexToTaskID(t, agentTaskIDHex)

	// Set the principal directly on the map (white-box; same package).
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals["agent-conn"] = tid

	if got := h.callerCaps("agent-conn"); got != parentCaps {
		t.Fatalf("agent caps = %#x, want %#x", got, parentCaps)
	}

	// Agent principal whose task is missing → Capability_None.
	var missingTID protocol.TaskID
	missingTID.Id[0] = 0xFF // non-zero so it's not treated as operator
	h.principals["ghost-conn"] = missingTID
	if got := h.callerCaps("ghost-conn"); got != protocol.Capability_None {
		t.Fatalf("ghost caps = %#x, want None (%#x)", got, protocol.Capability_None)
	}
}

// ---------------------------------------------------------------------------
// Task 3: spawn attenuation via handleSubmit
// ---------------------------------------------------------------------------

func TestSpawnAttenuation(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	// Register a runner so handleSubmit can resolve a candidate.
	h.Registry.Add(&RunnerEntry{
		ID:           "A",
		Hostname:     "runner-a",
		AllowedRoots: []string{"/x"},
		MaxTasks:     4,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  now,
		LastSeen:     now,
		Conn:         stubConn{},
	})

	// Create the parent task with a limited cap set (Spawn + FileRead).
	parentCaps := protocol.Capability_Spawn | protocol.Capability_FileRead
	parentIDHex := h.Tasks.Create("/x/repo", "parent", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Cli, protocol.TaskID{}, "A",
		protocol.RunnerSelector{}, nil, parentCaps, Scope{}, "")
	ptid := hexToTaskID(t, parentIDHex)

	// Wire the parent as the principal on "agent-conn".
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals["agent-conn"] = ptid

	// Case 1: request Capability_All from a limited parent → child gets parent's set.
	req1 := &protocol.SubmitRequest{RequestedCaps: protocol.Capability_All}
	req1.SetRepoPath([]byte("/x/repo"))
	req1.SetPrompt([]byte("child1"))

	resp1 := h.handleSubmit("", req1, protocol.ClientKind_Cli, ptid, h.callerCaps("agent-conn"))
	if resp1.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("case1 status=%v want Ok", resp1.Status)
	}
	child1IDHex := hex.EncodeToString(resp1.TaskId.Id[:])
	entry1, ok := h.Tasks.Get(child1IDHex)
	if !ok {
		t.Fatalf("child1 task %q not found", child1IDHex)
	}
	if entry1.Capabilities != parentCaps {
		t.Fatalf("child1 caps = %#x, want parent caps %#x", entry1.Capabilities, parentCaps)
	}

	// Case 2: request a cap the parent does NOT hold → child does not get it.
	req2 := &protocol.SubmitRequest{RequestedCaps: protocol.Capability_FileWrite}
	req2.SetRepoPath([]byte("/x/repo"))
	req2.SetPrompt([]byte("child2"))

	resp2 := h.handleSubmit("", req2, protocol.ClientKind_Cli, ptid, h.callerCaps("agent-conn"))
	if resp2.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("case2 status=%v want Ok", resp2.Status)
	}
	child2IDHex := hex.EncodeToString(resp2.TaskId.Id[:])
	entry2, ok := h.Tasks.Get(child2IDHex)
	if !ok {
		t.Fatalf("child2 task %q not found", child2IDHex)
	}
	if entry2.Capabilities != protocol.Capability_None {
		t.Fatalf("child2 caps = %#x, want None (parent lacks FileWrite)", entry2.Capabilities)
	}

	// Case 3: operator (connID not in principals) → child inherits full set.
	req3 := &protocol.SubmitRequest{RequestedCaps: protocol.Capability_All}
	req3.SetRepoPath([]byte("/x/repo"))
	req3.SetPrompt([]byte("child3"))

	operatorCaps := h.callerCaps("operator-conn") // not in principals → All
	resp3 := h.handleSubmit("", req3, protocol.ClientKind_Cli, protocol.TaskID{}, operatorCaps)
	if resp3.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("case3 status=%v want Ok", resp3.Status)
	}
	child3IDHex := hex.EncodeToString(resp3.TaskId.Id[:])
	entry3, ok := h.Tasks.Get(child3IDHex)
	if !ok {
		t.Fatalf("child3 task %q not found", child3IDHex)
	}
	if entry3.Capabilities != protocol.Capability_All {
		t.Fatalf("child3 caps = %#x, want All (operator creator)", entry3.Capabilities)
	}
}

// ---------------------------------------------------------------------------
// Task 4: Capability gate via requiredCap + denyTaskControl
// ---------------------------------------------------------------------------

// lastTaskControlResponse decodes the last message sent on conn as a
// TaskControlResponse (stripping the leading AppKind byte).
func lastTaskControlResponse(t *testing.T, conn *fakeConn) protocol.TaskControlResponse {
	t.Helper()
	msgs := conn.Sent()
	if len(msgs) == 0 {
		t.Fatal("no messages sent")
	}
	raw := msgs[len(msgs)-1]
	if len(raw) < 2 {
		t.Fatalf("message too short: %d bytes", len(raw))
	}
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(raw[1:]); err != nil {
		t.Fatalf("DecodeExact TaskControlResponse: %v", err)
	}
	return resp
}

// TestHandleDeniesWithoutCap: caller holds no caps, Cancel → PermissionDenied;
// victim task must NOT become Cancelled.
func TestHandleDeniesWithoutCap(t *testing.T) {
	h := newTestHandler(t)

	// Create the agent principal task holding NO caps.
	parentIDHex := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	ptid := hexToTaskID(t, parentIDHex)

	// Wire a caller conn with a distinct CID.
	callerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9600-1")}
	callerCID := callerConn.ConnectionID().String()
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[callerCID] = ptid

	// Victim task to attempt cancelling (with full caps — irrelevant; caller's caps are what matter).
	victimIDHex := h.Tasks.Create("repo", "v", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	vtid := hexToTaskID(t, victimIDHex)

	// Build and encode a Cancel request targeting the victim.
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel, RequestId: 7}
	req.SetCancel(protocol.CancelTask{TaskId: vtid})
	h.Handle(callerConn, encodeTaskControlRequest(t, req))

	// Victim must NOT be Cancelled.
	if vt, ok := h.Tasks.Get(victimIDHex); ok && vt.Status == protocol.TaskStatus_Cancelled {
		t.Fatal("cancel executed despite missing Cancel cap")
	}

	// Response must be PermissionDenied with correct fields.
	resp := lastTaskControlResponse(t, callerConn)
	if resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("resp.Kind = %v, want PermissionDenied", resp.Kind)
	}
	if resp.RequestId != 7 {
		t.Fatalf("resp.RequestId = %d, want 7", resp.RequestId)
	}
	pd := resp.PermissionDenied()
	if pd == nil {
		t.Fatal("PermissionDenied() returned nil")
	}
	if pd.RequiredCap != protocol.Capability_Cancel {
		t.Fatalf("pd.RequiredCap = %v, want Cancel", pd.RequiredCap)
	}
	if pd.RequestedKind != protocol.TaskControlKind_Cancel {
		t.Fatalf("pd.RequestedKind = %v, want Cancel", pd.RequestedKind)
	}
}

// TestHandleAllowsOperator: empty principals map → operator (Capability_All) →
// Cancel succeeds (victim becomes Cancelled, response Kind == Cancel).
func TestHandleAllowsOperator(t *testing.T) {
	h := newTestHandler(t)
	// No entry in h.principals → callerCaps returns Capability_All (operator).

	// Create a Running victim task.
	var rawID [16]byte
	rawID[0] = 0xBB
	victimIDHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[victimIDHex] = &TaskEntry{
		ID:       victimIDHex,
		RepoPath: "/r",
		Status:   protocol.TaskStatus_Running,
	}
	h.Tasks.order = append(h.Tasks.order, victimIDHex)
	h.Tasks.mu.Unlock()

	var vtid protocol.TaskID
	vtid.Id = rawID

	operatorConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9601-1")}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel, RequestId: 3}
	req.SetCancel(protocol.CancelTask{TaskId: vtid})
	h.Handle(operatorConn, encodeTaskControlRequest(t, req))

	// Victim must be Cancelled.
	if vt, ok := h.Tasks.Get(victimIDHex); !ok || vt.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("expected victim Cancelled, got status=%v ok=%v", func() interface{} {
			if entry, ok2 := h.Tasks.Get(victimIDHex); ok2 {
				return entry.Status
			}
			return "not found"
		}(), ok)
	}

	// Response must be Cancel (not PermissionDenied).
	resp := lastTaskControlResponse(t, operatorConn)
	if resp.Kind != protocol.TaskControlKind_Cancel {
		t.Fatalf("resp.Kind = %v, want Cancel", resp.Kind)
	}
}

// ---------------------------------------------------------------------------
// Task 5: direction-dependent capability gate
// ---------------------------------------------------------------------------

// makeAgentConn creates a fakeConn wired as an agent principal holding the
// given caps, returning the conn and the handler.
func makeAgentConn(t *testing.T, caps protocol.Capability) (*TaskHandler, *fakeConn) {
	t.Helper()
	h := newTestHandler(t)
	parentIDHex := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, caps, Scope{}, "")
	ptid := hexToTaskID(t, parentIDHex)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9700-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = ptid
	return h, conn
}

// assertPermissionDenied checks that the last response is a PermissionDenied
// with the expected required capability and that the RequestId is echoed.
func assertPermissionDenied(t *testing.T, conn *fakeConn, reqID uint32, wantCap protocol.Capability) {
	t.Helper()
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", resp.Kind)
	}
	if resp.RequestId != reqID {
		t.Fatalf("RequestId = %d, want %d", resp.RequestId, reqID)
	}
	pd := resp.PermissionDenied()
	if pd == nil {
		t.Fatal("PermissionDenied() returned nil")
	}
	if pd.RequiredCap != wantCap {
		t.Fatalf("RequiredCap = %v, want %v", pd.RequiredCap, wantCap)
	}
}

// assertNotPermissionDenied checks that the last response is NOT PermissionDenied
// (the gate passed; the underlying handler may still return an error).
func assertNotPermissionDenied(t *testing.T, conn *fakeConn) {
	t.Helper()
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("gate rejected the request unexpectedly (PermissionDenied)")
	}
}

// TestDirectionGate covers the direction-dependent gate cases (originally
// Task 5) plus the fixed-cap OpenPortForward case added when the port-forward
// registration gate moved to RegisterPortForward (see cases 5-7).
func TestDirectionGate(t *testing.T) {
	// Case 1: Pull without FileRead → denied (RequiredCap=FileRead).
	t.Run("file_pull_no_read_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileWrite) // has Write but not Read
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_OpenFileTransfer,
			RequestId: 11,
		}
		req.SetOpenFileTransfer(protocol.OpenFileTransferRequest{
			Direction: protocol.FileTransferDirection_Pull,
		})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 11, protocol.Capability_FileRead)
	})

	// Case 2: Push without FileWrite → denied (RequiredCap=FileWrite).
	t.Run("file_push_no_write_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileRead) // has Read but not Write
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_OpenFileTransfer,
			RequestId: 12,
		}
		req.SetOpenFileTransfer(protocol.OpenFileTransferRequest{
			Direction: protocol.FileTransferDirection_Push,
		})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 12, protocol.Capability_FileWrite)
	})

	// Case 3: ListFiles with only FileWrite → ALLOWED (floor: either file cap suffices).
	t.Run("file_ls_with_write_allowed", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileWrite) // Write but no Read
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_ListFiles,
			RequestId: 13,
		}
		req.SetListFiles(protocol.ListFilesRequest{})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertNotPermissionDenied(t, conn)
	})

	// Case 4: ListFiles with NEITHER file cap → denied (RequiredCap=FileRead as representative).
	t.Run("file_ls_no_caps_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_Spawn) // has Spawn only
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_ListFiles,
			RequestId: 14,
		}
		req.SetListFiles(protocol.ListFilesRequest{})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 14, protocol.Capability_FileRead)
	})

	// Case 5: register -L without ForwardLocal → denied (RequiredCap=ForwardLocal).
	// The direction-dependent gate lives on RegisterPortForward as of the
	// -R-onto-RegisterPortForward migration (OpenPortForward is now
	// unconditionally ForwardLocal; see forward_open_local_no_cap_denied below).
	t.Run("forward_local_no_cap_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileRead) // has FileRead only
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_RegisterPortForward,
			RequestId: 15,
		}
		req.SetRegisterPortForward(protocol.RegisterPortForwardRequest{
			Direction: protocol.PortForwardDirection_Local,
		})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 15, protocol.Capability_ForwardLocal)
	})

	// Case 6: register -R with only ForwardLocal → denied (RequiredCap=ForwardRemote).
	t.Run("forward_remote_only_local_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_ForwardLocal) // has ForwardLocal but not ForwardRemote
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_RegisterPortForward,
			RequestId: 16,
		}
		req.SetRegisterPortForward(protocol.RegisterPortForwardRequest{
			Direction: protocol.PortForwardDirection_Remote,
		})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 16, protocol.Capability_ForwardRemote)
	})

	// Case 7: per-connection OpenPortForward without ForwardLocal → denied.
	// OpenPortForward no longer carries a direction (it always means "open the
	// data stream for one accepted local-forward connection"), so its gate is a
	// fixed ForwardLocal requirement rather than direction-dependent.
	t.Run("forward_open_local_no_cap_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileRead) // has FileRead only
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_OpenPortForward,
			RequestId: 17,
		}
		req.SetOpenPortForward(protocol.OpenPortForwardRequest{})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 17, protocol.Capability_ForwardLocal)
	})

	// Case 8: kill a Local forward the caller CAN see (it is the caller's own
	// task), without ForwardLocal → denied (RequiredCap=ForwardLocal).
	// KillPortForward's request carries only a forward_id, not a direction, so
	// the gate cannot live in requiredCap like every other kind's: the dispatch
	// case looks the id up in the registry FIRST to learn its direction, resolves
	// visibility, and only THEN gates on the direction cap — visibility must be
	// checked before the capability, or a missing-cap denial for an out-of-subtree
	// id becomes an enumeration oracle (see case 11). This case pins the
	// "visible but under-capped" half of that split.
	t.Run("kill_local_no_cap_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileRead) // has FileRead only
		ownTID := h.lookupPrincipal(conn.ConnectionID().String())
		ownHex := hex.EncodeToString(ownTID.Id[:])
		id := h.pforwards().add(&portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: ownHex})
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_KillPortForward,
			RequestId: 18,
		}
		req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 18, protocol.Capability_ForwardLocal)
	})

	// Case 9: kill a Remote forward the caller CAN see, with only ForwardLocal →
	// denied (RequiredCap=ForwardRemote).
	t.Run("kill_remote_only_local_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_ForwardLocal) // has ForwardLocal but not ForwardRemote
		ownTID := h.lookupPrincipal(conn.ConnectionID().String())
		ownHex := hex.EncodeToString(ownTID.Id[:])
		id := h.pforwards().add(&portForward{direction: protocol.PortForwardDirection_Remote, taskIDHex: ownHex})
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_KillPortForward,
			RequestId: 19,
		}
		req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 19, protocol.Capability_ForwardRemote)
	})

	// Case 10: kill a Local forward the caller can see, WITH ForwardLocal →
	// gate passes AND the underlying kill actually succeeds (Ok), since the
	// forward is genuinely visible and correctly capped. Checking the final
	// status (not just "not denied") proves this reaches the real success path
	// rather than passing because some earlier branch silently swallowed it.
	t.Run("kill_local_with_cap_not_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_ForwardLocal)
		ownTID := h.lookupPrincipal(conn.ConnectionID().String())
		ownHex := hex.EncodeToString(ownTID.Id[:])
		id := h.pforwards().add(&portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: ownHex})
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_KillPortForward,
			RequestId: 20,
		}
		req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertNotPermissionDenied(t, conn)
		resp := lastTaskControlResponse(t, conn)
		kr := resp.KillPortForward()
		if kr == nil || kr.Status != protocol.KillPortForwardStatus_Ok {
			t.Fatalf("status = %+v, want ok", kr)
		}
	})

	// Case 11: kill a forward OUTSIDE the caller's subtree, with NEITHER
	// direction cap → no_such_forward, NOT PermissionDenied. This is the
	// enumeration-oracle guard itself: forward ids come from a dense next++
	// counter, so a PermissionDenied carrying the direction bit would let a
	// confined caller tell "exists, not mine" apart from "doesn't exist" and
	// learn which ids are live. Visibility must therefore be resolved before
	// the capability check ever runs.
	t.Run("kill_invisible_forward_no_such_forward_not_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_None) // no caps at all
		id := h.pforwards().add(&portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: "deadbeefdeadbeefdeadbeefdeadbeef"})
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_KillPortForward,
			RequestId: 21,
		}
		req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertNotPermissionDenied(t, conn)
		resp := lastTaskControlResponse(t, conn)
		if resp.Kind != protocol.TaskControlKind_KillPortForward {
			t.Fatalf("expected KillPortForward response, got %v", resp.Kind)
		}
		kr := resp.KillPortForward()
		if kr == nil || kr.Status != protocol.KillPortForwardStatus_NoSuchForward {
			t.Fatalf("status = %+v, want no_such_forward", kr)
		}
	})
}

// ---------------------------------------------------------------------------
// Task 6: visibleToCaller + handleList + handleGetTaskLog subtree scoping
// ---------------------------------------------------------------------------

// TestVisibleSubtree verifies the BFS descendant set:
//   - caller B (no InfoGlobal) sees itself + child C, not sibling D.
//   - caller B with InfoGlobal → all=true.
func TestVisibleSubtree(t *testing.T) {
	h := newTestHandler(t)

	// B: has Spawn but no InfoGlobal; no parent.
	bHex := h.Tasks.Create("r", "B", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")
	bTID := hexToTaskID(t, bHex)

	// C: child of B.
	cHex := h.Tasks.Create("r", "C", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		bTID, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")

	// D: sibling (no parent), unrelated to B.
	dHex := h.Tasks.Create("r", "D", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")

	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals["b-conn"] = bTID

	// B lacks InfoGlobal → subtree only.
	all, allowed := h.visibleToCaller("b-conn")
	if all {
		t.Fatal("B lacks InfoGlobal; all should be false")
	}
	if !allowed[bHex] {
		t.Errorf("B should see itself; bHex=%s allowed=%v", bHex, allowed)
	}
	if !allowed[cHex] {
		t.Errorf("B should see child C; cHex=%s allowed=%v", cHex, allowed)
	}
	if allowed[dHex] {
		t.Errorf("B should NOT see sibling D; dHex=%s allowed=%v", dHex, allowed)
	}

	// Now widen B's VISIBILITY rank -> all=true. This used to be a capability
	// bit (info_global); it is an axis of the scope now, so the grant that
	// produces the same reach is vis_base = global.
	h.Tasks.mu.Lock()
	h.Tasks.tasks[bHex].Scope = Scope{
		Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
	}
	h.Tasks.mu.Unlock()

	all2, _ := h.visibleToCaller("b-conn")
	if !all2 {
		t.Fatal("B with vis_base=global should have all=true")
	}
}

// TestListFilteredToSubtree verifies that handleList (via h.Handle) returns
// only the caller's subtree for a confined caller and all tasks for an operator.
func TestListFilteredToSubtree(t *testing.T) {
	h := newTestHandler(t)

	// Create two root tasks (operator-created).
	aHex := h.Tasks.Create("r", "A", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")
	_ = h.Tasks.Create("r", "B-root", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")

	// Create a child of A.
	aTID := hexToTaskID(t, aHex)
	aChildHex := h.Tasks.Create("r", "A-child", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		aTID, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")

	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}

	// -- Case 1: confined caller (task A, no InfoGlobal) ---
	callerConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9810-1"),
		nextSendStreamID: 10,
	}
	h.principals[callerConn.ConnectionID().String()] = aTID

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{})
	h.Handle(callerConn, encodeTaskControlRequest(t, req))

	if len(callerConn.sendStreams) != 1 {
		t.Fatalf("confined caller: expected 1 send stream, got %d", len(callerConn.sendStreams))
	}
	body := decodeListBody(t, callerConn.sendStreams[0].bytes)
	if body.TasksLen != 2 {
		// A itself + A-child
		t.Errorf("confined caller: expected 2 tasks (A + A-child), got %d", body.TasksLen)
	}
	taskHexes := taskIDsFromBody(t, body)
	if !taskHexes[aHex] {
		t.Errorf("confined caller: A must be visible; hex=%s", aHex)
	}
	if !taskHexes[aChildHex] {
		t.Errorf("confined caller: A-child must be visible; hex=%s", aChildHex)
	}

	// -- Case 2: operator (no entry in principals) sees all 3 tasks ---
	operatorConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9810-2"),
		nextSendStreamID: 11,
	}
	h.Handle(operatorConn, encodeTaskControlRequest(t, req))

	if len(operatorConn.sendStreams) != 1 {
		t.Fatalf("operator: expected 1 send stream, got %d", len(operatorConn.sendStreams))
	}
	opBody := decodeListBody(t, operatorConn.sendStreams[0].bytes)
	if opBody.TasksLen != 3 {
		t.Errorf("operator: expected 3 tasks, got %d", opBody.TasksLen)
	}
}

// TestListIncludesRedactedParent: `ls` / `session ls` grant a confined caller
// exactly ONE upward hop — its direct creator — and nothing above it. The
// parent row is redacted to lifecycle + liveness; the fields that describe
// where it runs and what it was asked to do are stripped. The ACCESS scope is
// unchanged: the parent's logs stay denied.
//
// Lineage: A (grandparent) → B (parent) → C (caller). S is unrelated.
func TestListIncludesRedactedParent(t *testing.T) {
	h := newTestHandler(t)
	h.LogsDir = t.TempDir() // so the log gate runs before the file open

	aHex := h.Tasks.Create("/repo/a", "grandparent prompt", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "claude")
	bHex := h.Tasks.Create("/repo/b", "parent prompt", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		hexToTaskID(t, aHex), "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "claude")
	bTID := hexToTaskID(t, bHex)
	cHex := h.Tasks.Create("/repo/c", "child prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		bTID, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "claude")
	sHex := h.Tasks.Create("/repo/s", "stranger prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

	// Give B a worktree + assigned runner so redaction has something to strip.
	h.Tasks.mu.Lock()
	h.Tasks.tasks[bHex].WorktreeDir = "/home/op/worktrees/b"
	h.Tasks.tasks[bHex].AssignedTo = "ws:127.0.0.1:8539-7"
	h.Tasks.tasks[bHex].ErrorMsg = []byte("boom in /home/op/worktrees/b")
	h.Tasks.tasks[bHex].Status = protocol.TaskStatus_Running
	h.Tasks.mu.Unlock()

	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	callerConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9830-1"),
		nextSendStreamID: 10,
	}
	h.principals[callerConn.ConnectionID().String()] = hexToTaskID(t, cHex)

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{})
	h.Handle(callerConn, encodeTaskControlRequest(t, req))

	if len(callerConn.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(callerConn.sendStreams))
	}
	body := decodeListBody(t, callerConn.sendStreams[0].bytes)
	seen := taskIDsFromBody(t, body)
	if !seen[cHex] {
		t.Errorf("caller must see itself; hex=%s", cHex)
	}
	if !seen[bHex] {
		t.Errorf("caller must see its direct parent B; hex=%s", bHex)
	}
	if seen[aHex] {
		t.Errorf("the hop is ONE level: grandparent A must stay hidden; hex=%s", aHex)
	}
	if seen[sHex] {
		t.Errorf("unrelated task S must stay hidden; hex=%s", sHex)
	}
	if body.TasksLen != 2 {
		t.Errorf("expected exactly 2 tasks (self + parent), got %d", body.TasksLen)
	}

	var parent, self *protocol.TaskInfo
	for i := range body.Tasks {
		switch hex.EncodeToString(body.Tasks[i].Id.Id[:]) {
		case bHex:
			parent = &body.Tasks[i]
		case cHex:
			self = &body.Tasks[i]
		}
	}
	if parent == nil || self == nil {
		t.Fatal("both the parent row and the caller's own row must be present")
	}

	// Redacted on the parent row: location + content.
	for _, f := range []struct {
		name string
		got  string
	}{
		{"prompt", string(parent.Prompt)},
		{"repo_path", string(parent.RepoPath)},
		{"worktree_dir", string(parent.WorktreeDir)},
		{"error_message", string(parent.ErrorMessage)},
		{"agent_profile", string(parent.AgentProfile)},
	} {
		if f.got != "" {
			t.Errorf("parent row must redact %s; got %q", f.name, f.got)
		}
	}
	if parent.AssignedTo.IpAddrLen != 0 || len(parent.AssignedTo.IpAddr) != 0 {
		t.Errorf("parent row must redact assigned_to; got %+v", parent.AssignedTo)
	}

	// Kept on the parent row: identity + lifecycle + liveness.
	if parent.Status != protocol.TaskStatus_Running {
		t.Errorf("parent row must keep status; got %v", parent.Status)
	}
	if parent.Kind != protocol.TaskKind_Interactive {
		t.Errorf("parent row must keep kind; got %v", parent.Kind)
	}
	if parent.CreatedAt == 0 {
		t.Error("parent row must keep created_at")
	}
	// Capabilities are NOT redacted: attenuation already tells the caller
	// caps_parent ⊇ caps_self, and a zeroed field would render as "none",
	// which is an outright false statement about the parent.
	if parent.Capabilities != protocol.Capability_All {
		t.Errorf("parent row must keep capabilities; got %v", parent.Capabilities)
	}

	// The caller's own row is untouched.
	if string(self.Prompt) != "child prompt" {
		t.Errorf("caller's own row must not be redacted; prompt=%q", string(self.Prompt))
	}
	if string(self.RepoPath) != "/repo/c" {
		t.Errorf("caller's own row must not be redacted; repo=%q", string(self.RepoPath))
	}

	// ACCESS scope unchanged: the parent's logs are still out of reach.
	logReq := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_GetTaskLog, RequestId: 77}
	logReq.SetGetLog(protocol.GetTaskLogRequest{TaskId: bTID})
	h.Handle(callerConn, encodeTaskControlRequest(t, logReq))
	resp := lastTaskControlResponse(t, callerConn)
	gl := resp.GetLog()
	if gl == nil {
		t.Fatal("GetLog() returned nil")
	}
	if gl.Found != 0 || gl.StreamId != 0 {
		t.Errorf("parent logs must stay denied; found=%d stream=%d", gl.Found, gl.StreamId)
	}
}

// TestListRunnersGatedByInfoGlobal: the RUNNERS section of List is gated by
// InfoGlobal exactly like agentHandleListTopics — a confined caller (no
// InfoGlobal, not operator) sees zero runners; operator and InfoGlobal-holders
// see the full runner list. Tasks remain subtree-filtered independently.
func TestListRunnersGatedByInfoGlobal(t *testing.T) {
	h := newTestHandler(t)

	// Register two runners so the global runner list is non-empty.
	runnerA := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-91")}
	runnerB := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-92")}
	h.Registry.Add(&RunnerEntry{
		ID:          runnerA.id.String(),
		Hostname:    "host-a",
		MaxTasks:    2,
		ActiveTasks: map[string]struct{}{},
		Conn:        runnerA,
	})
	h.Registry.Add(&RunnerEntry{
		ID:          runnerB.id.String(),
		Hostname:    "host-b",
		MaxTasks:    2,
		ActiveTasks: map[string]struct{}{},
		Conn:        runnerB,
	})

	// Confined caller: task A with Spawn but no InfoGlobal.
	aHex := h.Tasks.Create("r", "A", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")
	aTID := hexToTaskID(t, aHex)
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{})

	// -- Case 1: confined caller (no InfoGlobal) sees zero runners ---
	confinedConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9820-1"),
		nextSendStreamID: 10,
	}
	h.principals[confinedConn.ConnectionID().String()] = aTID
	h.Handle(confinedConn, encodeTaskControlRequest(t, req))
	if len(confinedConn.sendStreams) != 1 {
		t.Fatalf("confined caller: expected 1 send stream, got %d", len(confinedConn.sendStreams))
	}
	cBody := decodeListBody(t, confinedConn.sendStreams[0].bytes)
	if cBody.RunnersLen != 0 {
		t.Errorf("confined caller (no InfoGlobal): expected 0 runners, got %d", cBody.RunnersLen)
	}

	// -- Case 2: operator (no principal entry) sees both runners ---
	opConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9820-2"),
		nextSendStreamID: 11,
	}
	h.Handle(opConn, encodeTaskControlRequest(t, req))
	opBody := decodeListBody(t, opConn.sendStreams[0].bytes)
	if opBody.RunnersLen != 2 {
		t.Errorf("operator: expected 2 runners, got %d", opBody.RunnersLen)
	}

	// -- Case 3: confined caller with GLOBAL VISIBILITY sees both runners ---
	// The runner list rides visibleToCaller's all=true, which is task-space
	// visibility -- so the grant is the axis, not a capability bit.
	h.Tasks.tasks[aHex].Scope = Scope{
		Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
	}
	igConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9820-3"),
		nextSendStreamID: 12,
	}
	h.principals[igConn.ConnectionID().String()] = aTID
	h.Handle(igConn, encodeTaskControlRequest(t, req))
	igBody := decodeListBody(t, igConn.sendStreams[0].bytes)
	if igBody.RunnersLen != 2 {
		t.Errorf("caller with vis_base=global: expected 2 runners, got %d", igBody.RunnersLen)
	}
}

// decodeListBody decodes a ListResultBody from raw bytes recorded on a send stream.
func decodeListBody(t *testing.T, raw []byte) protocol.ListResultBody {
	t.Helper()
	var body protocol.ListResultBody
	if err := body.DecodeExact(raw); err != nil {
		t.Fatalf("decodeListBody: %v", err)
	}
	return body
}

// taskIDsFromBody returns a set of task id hex strings from the body.
func taskIDsFromBody(t *testing.T, body protocol.ListResultBody) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, ti := range body.Tasks {
		out[hex.EncodeToString(ti.Id.Id[:])] = true
	}
	return out
}

// TestGetTaskLogOutOfSubtreeDenied: a confined caller requesting logs of an
// out-of-subtree task receives the not-found response (found=0, streamId=0).
func TestGetTaskLogOutOfSubtreeDenied(t *testing.T) {
	h := newTestHandler(t)
	h.LogsDir = t.TempDir() // enable log path so the gate runs before the open

	aHex := h.Tasks.Create("r", "A", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")
	aTID := hexToTaskID(t, aHex)

	// D is an unrelated task (operator-created).
	dHex := h.Tasks.Create("r", "D", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	dTID := hexToTaskID(t, dHex)

	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	callerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9811-1")}
	h.principals[callerConn.ConnectionID().String()] = aTID

	// Request logs for task D (out of A's subtree).
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_GetTaskLog, RequestId: 42}
	req.SetGetLog(protocol.GetTaskLogRequest{TaskId: dTID})
	h.Handle(callerConn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, callerConn)
	if resp.Kind != protocol.TaskControlKind_GetTaskLog {
		t.Fatalf("expected GetTaskLog response kind, got %v", resp.Kind)
	}
	gl := resp.GetLog()
	if gl == nil {
		t.Fatal("GetLog() returned nil")
	}
	if gl.Found != 0 {
		t.Errorf("expected Found=0 (denied), got %d", gl.Found)
	}
	if gl.StreamId != 0 {
		t.Errorf("expected StreamId=0 (denied), got %d", gl.StreamId)
	}
}

// ---------------------------------------------------------------------------
// Task 6: agentCallerCaps + TestTopicsGated
// ---------------------------------------------------------------------------

// makeTestAgentConn builds a *Server with tasks + board and an *agentConn
// with helloed=true and an identity backed by a task in s.tasks holding caps.
// The agentConn.state.Identity() TaskID will match the created task.
func makeTestAgentConn(t *testing.T, caps protocol.Capability) (*Server, *agentConn) {
	t.Helper()
	board := newTestBoard(t)
	tasks := NewTaskStore()

	// Create a task with the desired capability set.
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0x10, 0x20, 0x30, 0x40}
	tidHex := hex.EncodeToString(protoTID.Id[:])
	tasks.mu.Lock()
	tasks.tasks[tidHex] = &TaskEntry{
		ID:           tidHex,
		RepoPath:     "r",
		Capabilities: caps,
	}
	tasks.order = append(tasks.order, tidHex)
	tasks.mu.Unlock()

	// Build agentboard RunnerID/TaskID and Attach to get a ConnState.
	var boardRID agentboard.RunnerID
	boardRID.SetTransport([]byte("ws"))
	boardRID.SetIpAddr([]byte{127, 0, 0, 1}) // IPv4 placeholder (IpAddrLen constraint)
	boardRID.Port = 8539
	boardRID.UniqueNumber = 1

	var boardTID agentboard.TaskID
	copy(boardTID.Id[:], protoTID.Id[:])

	state := board.Attach(boardRID, boardTID, "testhost", "")

	ac := &agentConn{
		state:   state,
		helloed: true,
	}

	s := &Server{
		Board: board,
		tasks: tasks,
	}
	return s, ac
}

// TestAgentCallerCaps verifies that agentCallerCaps resolves the task's
// Capabilities from s.tasks using the TaskID from ac.state.Identity().
func TestAgentCallerCaps(t *testing.T) {
	s, ac := makeTestAgentConn(t, protocol.Capability_Spawn|protocol.Capability_FileRead)
	got := s.agentCallerCaps(ac)
	want := protocol.Capability_Spawn | protocol.Capability_FileRead
	if got != want {
		t.Fatalf("agentCallerCaps = %#x, want %#x", got, want)
	}

	// nil agentConn → Capability_None.
	if got2 := s.agentCallerCaps(nil); got2 != protocol.Capability_None {
		t.Fatalf("agentCallerCaps(nil) = %#x, want None", got2)
	}
}

// TestTopicsGated verifies the InfoGlobal gate on agentHandleListTopics:
//   - caller without InfoGlobal → Status_Denied and zero topics.
//   - caller with InfoGlobal → Status_Ok and topics returned (Board has one
//     published topic).
//
// Status is asserted here, not only through the CLI: zero topics is also the
// honest answer for an empty board, so the status byte is the only thing on
// the wire that separates "refused" from "nothing to show".
func TestTopicsGated(t *testing.T) {
	// Helper: decode the ListTopicsResponse from the last sent agent message.
	decodeListTopicsResp := func(t *testing.T, conn *fakeConn) *agentboard.ListTopicsResponse {
		t.Helper()
		msgs := conn.Sent()
		if len(msgs) == 0 {
			t.Fatal("no messages sent")
		}
		raw := msgs[len(msgs)-1]
		if len(raw) < 2 {
			t.Fatalf("message too short: %d bytes", len(raw))
		}
		var msg agentboard.AgentMessage
		if err := msg.DecodeExact(raw[1:]); err != nil {
			t.Fatalf("DecodeExact AgentMessage: %v", err)
		}
		r := msg.ListTopicsResponse()
		if r == nil {
			t.Fatal("ListTopicsResponse() returned nil")
		}
		return r
	}

	// Publish a topic so the board is non-empty for the InfoGlobal case.
	publishToBoard := func(t *testing.T, board *agentboard.Board) {
		t.Helper()
		var fromRID protocol.RunnerID
		fromRID.SetTransport([]byte("ws"))
		fromRID.SetIpAddr([]byte{127, 0, 0, 2})
		fromRID.Port = 8540
		fromRID.UniqueNumber = 2
		var fromTID protocol.TaskID
		fromTID.Id[0] = 0xFF
		_, _, _ = board.Send("test.topic", []byte("hello"), fromRID, fromTID, "testhost", "", 0)
	}

	// Case 1: no InfoGlobal → zero topics.
	t.Run("no_info_global_zero_topics", func(t *testing.T) {
		s, ac := makeTestAgentConn(t, protocol.Capability_Spawn) // no InfoGlobal
		publishToBoard(t, s.Board)
		conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9820-1")}
		req := &agentboard.ListTopicsRequest{RequestId: 1}
		s.agentHandleListTopics(conn, ac, req)
		resp := decodeListTopicsResp(t, conn)
		if resp.Status != agentboard.ListTopicsStatus_Denied {
			t.Errorf("expected Status_Denied without InfoGlobal, got %v", resp.Status)
		}
		if resp.TopicsLen != 0 || len(resp.Topics) != 0 {
			t.Errorf("expected 0 topics without InfoGlobal, got TopicsLen=%d Topics=%v",
				resp.TopicsLen, resp.Topics)
		}
	})

	// Case 2: with InfoGlobal → topics returned.
	t.Run("info_global_sees_topics", func(t *testing.T) {
		s, ac := makeTestAgentConn(t, protocol.Capability_BoardObserve)
		publishToBoard(t, s.Board)
		conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9820-2")}
		req := &agentboard.ListTopicsRequest{RequestId: 2}
		s.agentHandleListTopics(conn, ac, req)
		resp := decodeListTopicsResp(t, conn)
		if resp.Status != agentboard.ListTopicsStatus_Ok {
			t.Errorf("expected Status_Ok with InfoGlobal, got %v", resp.Status)
		}
		if resp.TopicsLen == 0 {
			t.Error("expected non-zero topics with InfoGlobal")
		}
	})
}

// ---------------------------------------------------------------------------
// Task 2: TestResumeCapsOverride
// ---------------------------------------------------------------------------

// markTerminalForTest drives a task to a terminal state (Succeeded) by
// assigning then finishing it. Mirrors the pattern used in resume_test.go.
func markTerminalForTest(t *testing.T, h *TaskHandler, idHex string) {
	t.Helper()
	h.Tasks.Assign(idHex, "runner-x", "/wt/x", false)
	h.Tasks.Finish(idHex, 0, nil)
	e, ok := h.Tasks.Get(idHex)
	if !ok {
		t.Fatalf("markTerminalForTest: task %q not found", idHex)
	}
	if e.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("markTerminalForTest: status = %v, want Succeeded", e.Status)
	}
}

// TestResumeCapsOverride covers:
//  1. operator override → caps replaced by intersect(All, requested) = requested.
//  2. plain resume (override=false) → caps UNCHANGED (regression guard).
//  3. limited agent override → caps = intersect(agentCaps, requested).
func TestResumeCapsOverride(t *testing.T) {
	// -----------------------------------------------------------------------
	// Case 1: operator override resume → caps replaced
	// -----------------------------------------------------------------------
	t.Run("operator_override_replaces_caps", func(t *testing.T) {
		h := newTestHandler(t)
		id := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")

		markTerminalForTest(t, h, id)

		// Operator caller: not in h.principals → callerCaps = Capability_All.
		// override=true, requested=FileRead → intersect(All, FileRead) = FileRead.
		if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
			true, intersectCaps(protocol.Capability_All, protocol.Capability_FileRead), false, Scope{}, protocol.TaskKind_Oneshot, ""); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		e, ok := h.Tasks.Get(id)
		if !ok {
			t.Fatalf("task %q not found after override resume", id)
		}
		if e.Capabilities != protocol.Capability_FileRead {
			t.Fatalf("override caps = %#x, want FileRead (%#x)", e.Capabilities, protocol.Capability_FileRead)
		}
	})

	// -----------------------------------------------------------------------
	// Case 2: plain resume (override=false) → caps unchanged
	// -----------------------------------------------------------------------
	t.Run("plain_resume_caps_unchanged", func(t *testing.T) {
		h := newTestHandler(t)
		wantCaps := protocol.Capability_Spawn | protocol.Capability_FileRead
		id := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, wantCaps, Scope{}, "")

		markTerminalForTest(t, h, id)

		// override=false → Capabilities must stay wantCaps regardless of newCaps arg.
		if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
			false, protocol.Capability_None, false, Scope{}, protocol.TaskKind_Oneshot, ""); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		e, ok := h.Tasks.Get(id)
		if !ok {
			t.Fatalf("task %q not found after plain resume", id)
		}
		if e.Capabilities != wantCaps {
			t.Fatalf("plain resume changed caps to %#x, want %#x", e.Capabilities, wantCaps)
		}
	})

	// -----------------------------------------------------------------------
	// Case 3: limited agent override → caps = intersect(agentCaps, requested)
	// -----------------------------------------------------------------------
	t.Run("limited_agent_override_intersects", func(t *testing.T) {
		h := newTestHandler(t)

		// Create the agent task holding limited caps (Spawn + FileRead).
		agentCaps := protocol.Capability_Spawn | protocol.Capability_FileRead
		agentTaskIDHex := h.Tasks.Create("/r", "agent", protocol.TaskKind_Oneshot,
			protocol.ClientKind_Agent, protocol.TaskID{}, "",
			protocol.RunnerSelector{}, nil, agentCaps, Scope{}, "")
		agentTID := hexToTaskID(t, agentTaskIDHex)

		// Create the target task (original caps = All).
		targetID := h.Tasks.Create("/r", "target", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
		markTerminalForTest(t, h, targetID)

		// Wire the agent as a principal on a distinct conn.
		if h.principals == nil {
			h.principals = make(map[string]protocol.TaskID)
		}
		const agentConnID = "ws:127.0.0.1:9900-1"
		h.principals[agentConnID] = agentTID

		// Agent requests All caps with override=true.
		// intersect(agentCaps, All) = agentCaps (agent cannot widen).
		callerCaps := h.callerCaps(agentConnID)
		newCaps := intersectCaps(callerCaps, protocol.Capability_All)
		if _, err := h.Tasks.Resume(targetID, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Agent,
			true, newCaps, false, Scope{}, protocol.TaskKind_Oneshot, ""); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		e, ok := h.Tasks.Get(targetID)
		if !ok {
			t.Fatalf("task %q not found after agent override resume", targetID)
		}
		if e.Capabilities != agentCaps {
			t.Fatalf("agent override caps = %#x, want agentCaps %#x", e.Capabilities, agentCaps)
		}

		// Verify: agent requesting a cap it lacks → that cap is stripped.
		// FileWrite is NOT in agentCaps → intersect(agentCaps, FileWrite) = None.
		markTerminalForTest(t, h, targetID)
		newCaps2 := intersectCaps(callerCaps, protocol.Capability_FileWrite)
		if _, err := h.Tasks.Resume(targetID, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Agent,
			true, newCaps2, false, Scope{}, protocol.TaskKind_Oneshot, ""); err != nil {
			t.Fatalf("Resume2: %v", err)
		}
		e2, _ := h.Tasks.Get(targetID)
		if e2.Capabilities != protocol.Capability_None {
			t.Fatalf("agent lacked FileWrite; caps should be None, got %#x", e2.Capabilities)
		}
	})
}

func TestRequiredCap_BoardSubscribers(t *testing.T) {
	got, ok := requiredCap[protocol.TaskControlKind_BoardSubscribers]
	if !ok {
		t.Fatal("board_subscribers missing from requiredCap; an ungated kind is reachable by any helloed agent")
	}
	if got != protocol.Capability_BoardObserve {
		t.Errorf("cap = %v, want board_observe (matching board_topics / board_read)", got)
	}
}

// ---- scope resolution (spec §2 / §2a) ------------------------------------

// scopeFixture builds: P (root) → C (child of P) → G (grandchild of C), plus an
// unrelated task U. Returns the handler and the four id hexes.
func scopeFixture(t *testing.T) (h *TaskHandler, p, c, g, u string) {
	t.Helper()
	h = &TaskHandler{Tasks: NewTaskStore()}
	// Deliberately NOT Capability_All: that includes info_global, which makes
	// visibleToCaller answer all=true and quietly voids every scope assertion
	// below. Tests that want it grant it explicitly.
	caps := protocol.Capability_All &^ protocol.Capability_BoardObserve
	mk := func(prompt string, creator protocol.TaskID) string {
		return h.Tasks.Create("/r", prompt, protocol.TaskKind_Oneshot,
			protocol.ClientKind_Agent, creator, "", protocol.RunnerSelector{},
			nil, caps, Scope{}, "")
	}
	p = mk("p", protocol.TaskID{})
	c = mk("c", hexToTaskID(t, p))
	g = mk("g", hexToTaskID(t, c))
	u = mk("u", protocol.TaskID{})
	return h, p, c, g, u
}

// bindPrincipal points a fake connection id at a task and returns the conn id.
func bindPrincipal(t *testing.T, h *TaskHandler, taskHex string) string {
	t.Helper()
	cid := "ws:127.0.0.1:9999-" + taskHex[:4]
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[cid] = hexToTaskID(t, taskHex)
	return cid
}

func setScope(t *testing.T, h *TaskHandler, taskHex string, sc Scope) {
	t.Helper()
	if _, ok := h.Tasks.SetCaps(taskHex, false, 0, true, sc); !ok {
		t.Fatalf("SetCaps on %s: task not found", taskHex)
	}
}

func TestScopeSetHonoursBaseAndIDs(t *testing.T) {
	h, p, c, g, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)

	// subtree (the default): self + descendants, not the parent, not a stranger.
	all, allowed := h.scopeSet(cid, protocol.Capability_Cancel)
	if all || !allowed[c] || !allowed[g] || allowed[p] || allowed[u] {
		t.Errorf("subtree: all=%v allowed=%v, want {c,g} only", all, allowed)
	}

	// none: self only. Descendants are NOT reachable.
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None})
	all, allowed = h.scopeSet(cid, protocol.Capability_Cancel)
	if all || !allowed[c] || allowed[g] || allowed[p] || allowed[u] {
		t.Errorf("none: all=%v allowed=%v, want {c} only", all, allowed)
	}

	// none + ids: self plus exactly the named strangers.
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None, IDs: []string{u}})
	all, allowed = h.scopeSet(cid, protocol.Capability_Cancel)
	if all || !allowed[c] || !allowed[u] || allowed[g] {
		t.Errorf("none+ids: all=%v allowed=%v, want {c,u}", all, allowed)
	}

	// subtree + ids: both.
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{u}})
	all, allowed = h.scopeSet(cid, protocol.Capability_Cancel)
	if all || !allowed[c] || !allowed[g] || !allowed[u] {
		t.Errorf("subtree+ids: all=%v allowed=%v, want {c,g,u}", all, allowed)
	}

	// global: everything, allowed nil.
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Global})
	if all, allowed = h.scopeSet(cid, protocol.Capability_Cancel); !all || allowed != nil {
		t.Errorf("global: all=%v allowed=%v, want true/nil", all, allowed)
	}
}

func TestAuthorizeRequiresBothCapAndScope(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)

	// Cap held, target out of scope.
	if h.authorize(cid, protocol.Capability_Cancel, u) {
		t.Error("authorized a stranger while scope is subtree")
	}
	// Target in scope, cap missing.
	h.Tasks.SetCaps(c, true, protocol.Capability_FileRead, false, Scope{}) //nolint:errcheck
	if h.authorize(cid, protocol.Capability_Cancel, c) {
		t.Error("authorized cancel without the cancel bit")
	}
	// Both.
	h.Tasks.SetCaps(c, true, protocol.Capability_Cancel, false, Scope{}) //nolint:errcheck
	if !h.authorize(cid, protocol.Capability_Cancel, c) {
		t.Error("denied a self-target with the cap held")
	}
	// Operator: no principal, everything.
	if !h.authorize("ws:127.0.0.1:1-0", protocol.Capability_Cancel, u) {
		t.Error("denied an operator")
	}
}

// A wide visibility rank widens what may be SEEN and must not widen what may
// be DONE. The base spec protected this by keeping info_global out of
// scopeSet; it is now a property of two fields of one value, which is the same
// asymmetry with the fence removed.
func TestVisBaseWidensVisibilityNotAction(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	h.Tasks.SetCaps(c, true, protocol.Capability_Cancel, true, Scope{ //nolint:errcheck
		Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
	})

	if all, _ := h.visibleToCaller(cid); !all {
		t.Error("vis_base=global did not widen visibility")
	}
	if all, allowed := h.scopeSet(cid, protocol.Capability_Cancel); all || allowed[u] {
		t.Error("the visibility rank leaked into the ACTION set")
	}
	if h.authorize(cid, protocol.Capability_Cancel, u) {
		t.Error("vis_base=global + cancel authorized a stranger")
	}
}

// Spec §2a: the LIST-only parent hop is justified by the creator relationship,
// so it survives the narrowest base — and never becomes an authorize target.
func TestParentHopSurvivesNoneBaseAndIsNotActionable(t *testing.T) {
	h, p, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None})

	all, allowed, parentHex := h.listVisibleToCaller(cid)
	if all {
		t.Fatal("a scope=none caller should not see everything")
	}
	if parentHex != p {
		t.Errorf("parentHex = %q, want the creator %q", parentHex, p)
	}
	if allowed[p] {
		t.Error("the parent leaked into the ACCESS set")
	}
	if h.authorize(cid, protocol.Capability_Cancel, p) {
		t.Error("the parent hop made the parent actionable")
	}
}

// An explicit ids:<parent> grant is the one way the parent becomes actionable,
// and then it is a full row rather than a redacted hop.
func TestParentInIDsIsActionableAndUnredacted(t *testing.T) {
	h, p, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None, IDs: []string{p}})

	if !h.authorize(cid, protocol.Capability_Cancel, p) {
		t.Error("an explicit ids:<parent> grant did not authorize the parent")
	}
	_, allowed, parentHex := h.listVisibleToCaller(cid)
	if !allowed[p] {
		t.Error("the parent is not in the ACCESS set despite being named")
	}
	if parentHex != "" {
		t.Errorf("parentHex = %q, want empty — the parent is already in allowed "+
			"so it must be listed unredacted, not as a hop", parentHex)
	}
}

// ---- per-capability resolution (2026-08-20 design §2, §4) -----------------

// An override binds the bit in its mask and nothing else, and it never narrows
// what ls shows. Both halves matter: the first is the feature, the second is
// the invariant that keeps a caller from acting on what it cannot see.
func TestScopeSetPerCapability(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{
		Base:      protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}},
	})

	if _, allowed := h.scopeSet(cid, protocol.Capability_ExecView); !allowed[g] {
		t.Error("exec_view lost the grandchild; only cancel was overridden")
	}
	if _, allowed := h.scopeSet(cid, protocol.Capability_Cancel); allowed[g] {
		t.Error("cancel reached the grandchild despite an override of base none")
	}
	if _, allowed := h.visibleToCaller(cid); !allowed[g] {
		t.Error("visibility narrowed by an action override; overrides bind actions only")
	}
}

// exclude_self removes self from ONE action set. It must not remove self from
// visibility, and must not touch another capability.
func TestExcludeSelfIsPerCapabilityAndNeverHidesSelf(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{
		Base: protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{
			Caps: protocol.Capability_ExecCowrite, Base: protocol.ScopeBase_Subtree, ExcludeSelf: true,
		}},
	})

	if _, allowed := h.scopeSet(cid, protocol.Capability_ExecCowrite); allowed[c] {
		t.Error("exec_cowrite still reaches self; exclude_self did not apply")
	}
	if _, allowed := h.scopeSet(cid, protocol.Capability_ExecCowrite); !allowed[g] {
		t.Error("exclude_self also dropped the descendants; it must remove self alone")
	}
	if _, allowed := h.scopeSet(cid, protocol.Capability_ExecView); !allowed[c] {
		t.Error("exec_view lost self; exclude_self must not leak to another bit")
	}
	if _, allowed := h.visibleToCaller(cid); !allowed[c] {
		t.Error("self vanished from ls; seeing your own row is orientation, not authority")
	}
}

// A granted id is a disclosed id: it joins the visible set without being
// repeated in vis_ids, which is what lets an override name targets outside the
// base while "never act on what ls denies" still holds.
func TestOverrideIDsAreVisible(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{
		Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None,
		Overrides: []ScopeOverride{{
			Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None, IDs: []string{u},
		}},
	})

	if _, allowed := h.scopeSet(cid, protocol.Capability_Cancel); !allowed[u] {
		t.Error("cancel cannot reach the id its own override names")
	}
	if _, allowed := h.scopeSet(cid, protocol.Capability_ExecView); allowed[u] {
		t.Error("exec_view reached an id only cancel was granted")
	}
	if _, allowed := h.visibleToCaller(cid); !allowed[u] {
		t.Error("a granted id is not visible; it was disclosed by the granter and must be listed")
	}
}

// vis_ids are seen and never actionable -- the "watch this sibling, touch
// nothing" grant that otherwise needs an action id plus an override on every
// held bit, where one forgotten bit reaches it.
func TestVisIDsAreVisibleButNotActionable(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}})

	if _, allowed := h.visibleToCaller(cid); !allowed[u] {
		t.Error("vis_ids target is not visible")
	}
	for _, bit := range []protocol.Capability{
		protocol.Capability_Cancel, protocol.Capability_ExecView, protocol.Capability_FileWrite,
	} {
		if _, allowed := h.scopeSet(cid, bit); allowed[u] {
			t.Errorf("%v reached a view-only target", bit)
		}
	}
}

// vis_base widens what may be SEEN without widening what may be DONE -- the
// asymmetry the base spec kept info_global out of scopeSet to protect, now a
// property of the value rather than a fence around a bit.
func TestVisBaseWidensSightNotAction(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{
		Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
	})

	if all, _ := h.visibleToCaller(cid); !all {
		t.Error("vis_base global did not widen visibility")
	}
	if all, allowed := h.scopeSet(cid, protocol.Capability_Cancel); all || allowed[u] {
		t.Errorf("cancel reached an unrelated task: all=%v allowed[u]=%v", all, allowed[u])
	}
}
