package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
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
