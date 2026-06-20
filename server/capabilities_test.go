package server

import (
	"encoding/hex"
	"testing"
	"time"

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
		protocol.RunnerSelector{}, nil, parentCaps)
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
		protocol.RunnerSelector{}, nil, parentCaps)
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

	resp1 := h.handleSubmit(req1, protocol.ClientKind_Cli, ptid, h.callerCaps("agent-conn"))
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

	resp2 := h.handleSubmit(req2, protocol.ClientKind_Cli, ptid, h.callerCaps("agent-conn"))
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
	resp3 := h.handleSubmit(req3, protocol.ClientKind_Cli, protocol.TaskID{}, operatorCaps)
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
		protocol.RunnerSelector{}, nil, protocol.Capability_None)
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
		protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
		protocol.RunnerSelector{}, nil, caps)
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

// TestDirectionGate covers the six direction-dependent gate cases (Task 5).
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

	// Case 5: forward -L without ForwardLocal → denied (RequiredCap=ForwardLocal).
	t.Run("forward_local_no_cap_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_FileRead) // has FileRead only
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_OpenPortForward,
			RequestId: 15,
		}
		req.SetOpenPortForward(protocol.OpenPortForwardRequest{
			Direction: protocol.PortForwardDirection_Local,
		})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 15, protocol.Capability_ForwardLocal)
	})

	// Case 6: forward -R with only ForwardLocal → denied (RequiredCap=ForwardRemote).
	t.Run("forward_remote_only_local_denied", func(t *testing.T) {
		h, conn := makeAgentConn(t, protocol.Capability_ForwardLocal) // has ForwardLocal but not ForwardRemote
		req := &protocol.TaskControlRequest{
			Kind:      protocol.TaskControlKind_OpenPortForward,
			RequestId: 16,
		}
		req.SetOpenPortForward(protocol.OpenPortForwardRequest{
			Direction: protocol.PortForwardDirection_Remote,
		})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		assertPermissionDenied(t, conn, 16, protocol.Capability_ForwardRemote)
	})
}
