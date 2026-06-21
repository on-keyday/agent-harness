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
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	bTID := hexToTaskID(t, bHex)

	// C: child of B.
	cHex := h.Tasks.Create("r", "C", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		bTID, "", protocol.RunnerSelector{}, nil, protocol.Capability_None)

	// D: sibling (no parent), unrelated to B.
	dHex := h.Tasks.Create("r", "D", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None)

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

	// Now give B InfoGlobal → all=true.
	h.Tasks.mu.Lock()
	h.Tasks.tasks[bHex].Capabilities = protocol.Capability_InfoGlobal
	h.Tasks.mu.Unlock()

	all2, _ := h.visibleToCaller("b-conn")
	if !all2 {
		t.Fatal("B with InfoGlobal should have all=true")
	}
}

// TestListFilteredToSubtree verifies that handleList (via h.Handle) returns
// only the caller's subtree for a confined caller and all tasks for an operator.
func TestListFilteredToSubtree(t *testing.T) {
	h := newTestHandler(t)

	// Create two root tasks (operator-created).
	aHex := h.Tasks.Create("r", "A", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	_ = h.Tasks.Create("r", "B-root", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)

	// Create a child of A.
	aTID := hexToTaskID(t, aHex)
	aChildHex := h.Tasks.Create("r", "A-child", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		aTID, "", protocol.RunnerSelector{}, nil, protocol.Capability_None)

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
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
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

	// -- Case 3: confined caller WITH InfoGlobal sees both runners ---
	h.Tasks.tasks[aHex].Capabilities = protocol.Capability_InfoGlobal
	igConn := &fakeConn{
		id:               objproto.MustParseConnectionID("ws:127.0.0.1:9820-3"),
		nextSendStreamID: 12,
	}
	h.principals[igConn.ConnectionID().String()] = aTID
	h.Handle(igConn, encodeTaskControlRequest(t, req))
	igBody := decodeListBody(t, igConn.sendStreams[0].bytes)
	if igBody.RunnersLen != 2 {
		t.Errorf("caller with InfoGlobal: expected 2 runners, got %d", igBody.RunnersLen)
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
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)
	aTID := hexToTaskID(t, aHex)

	// D is an unrelated task (operator-created).
	dHex := h.Tasks.Create("r", "D", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None)
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

	state := board.Attach(boardRID, boardTID, "testhost")

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
//   - caller without InfoGlobal → zero topics returned.
//   - caller with InfoGlobal → topics returned (Board has one published topic).
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
		_, _ = board.Send("test.topic", []byte("hello"), fromRID, fromTID, "testhost")
	}

	// Case 1: no InfoGlobal → zero topics.
	t.Run("no_info_global_zero_topics", func(t *testing.T) {
		s, ac := makeTestAgentConn(t, protocol.Capability_Spawn) // no InfoGlobal
		publishToBoard(t, s.Board)
		conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9820-1")}
		req := &agentboard.ListTopicsRequest{RequestId: 1}
		s.agentHandleListTopics(conn, ac, req)
		resp := decodeListTopicsResp(t, conn)
		if resp.TopicsLen != 0 || len(resp.Topics) != 0 {
			t.Errorf("expected 0 topics without InfoGlobal, got TopicsLen=%d Topics=%v",
				resp.TopicsLen, resp.Topics)
		}
	})

	// Case 2: with InfoGlobal → topics returned.
	t.Run("info_global_sees_topics", func(t *testing.T) {
		s, ac := makeTestAgentConn(t, protocol.Capability_InfoGlobal)
		publishToBoard(t, s.Board)
		conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9820-2")}
		req := &agentboard.ListTopicsRequest{RequestId: 2}
		s.agentHandleListTopics(conn, ac, req)
		resp := decodeListTopicsResp(t, conn)
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
	h.Tasks.Assign(idHex, "runner-x", "/wt/x")
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
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn)

		markTerminalForTest(t, h, id)

		// Operator caller: not in h.principals → callerCaps = Capability_All.
		// override=true, requested=FileRead → intersect(All, FileRead) = FileRead.
		if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
			true, intersectCaps(protocol.Capability_All, protocol.Capability_FileRead)); err != nil {
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
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, wantCaps)

		markTerminalForTest(t, h, id)

		// override=false → Capabilities must stay wantCaps regardless of newCaps arg.
		if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
			false, protocol.Capability_None); err != nil {
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
			protocol.RunnerSelector{}, nil, agentCaps)
		agentTID := hexToTaskID(t, agentTaskIDHex)

		// Create the target task (original caps = All).
		targetID := h.Tasks.Create("/r", "target", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
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
			true, newCaps); err != nil {
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
			true, newCaps2); err != nil {
			t.Fatalf("Resume2: %v", err)
		}
		e2, _ := h.Tasks.Get(targetID)
		if e2.Capabilities != protocol.Capability_None {
			t.Fatalf("agent lacked FileWrite; caps should be None, got %#x", e2.Capabilities)
		}
	})
}
