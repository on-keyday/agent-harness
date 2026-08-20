package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// prune reaches every task on the server unless it is filtered. Before this,
// the only thing stopping an agent from forgetting the operator's task history
// was a paragraph in the supervising-workers skill asking it not to.
func TestPruneIsScopedToTheCaller(t *testing.T) {
	h, _, c, g, u := scopeFixture(t)
	dir := t.TempDir()
	h.PruneFn = func(allowed map[string]bool, req *protocol.PruneTasksRequest) (int, int, int) {
		if req.TaskIdsLen == 0 {
			return h.Tasks.PruneTerminal(allowed, time.Unix(0, int64(req.BeforeTs)), dir), 0, 0
		}
		ids := make([]string, 0, req.TaskIdsLen)
		for i := range req.TaskIds {
			ids = append(ids, hexOf(req.TaskIds[i]))
		}
		return h.Tasks.PruneByIDs(allowed, ids, req.Force != 0, dir)
	}
	// Make the descendant and the stranger terminal so both are prunable.
	for _, id := range []string{g, u} {
		h.Tasks.Assign(id, "runner-x", "/wt", false)
		h.Tasks.Finish(id, 0, nil)
	}

	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9950-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, c)

	t.Run("by ids", func(t *testing.T) {
		req := protocol.PruneTasksRequest{TaskIdsLen: 2, TaskIds: []protocol.TaskID{
			hexToTaskID(t, g), hexToTaskID(t, u),
		}}
		tc := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_PruneTasks, RequestId: 1}
		tc.SetPrune(req)
		h.Handle(conn, encodeTaskControlRequest(t, tc))

		resp := lastTaskControlResponse(t, conn)
		got := resp.Prune()
		if got == nil || got.Removed != 1 || got.SkippedMissing != 1 {
			t.Fatalf("removed=%v, want 1 removed + 1 skipped_missing", got)
		}
		if _, ok := h.Tasks.Get(u); !ok {
			t.Error("the out-of-scope task was pruned")
		}
		if _, ok := h.Tasks.Get(g); ok {
			t.Error("the in-scope descendant was not pruned")
		}
	})

	t.Run("bare age sweep", func(t *testing.T) {
		// The dangerous form: no ids, sweep everything older than the cutoff.
		req := protocol.PruneTasksRequest{BeforeTs: uint64(time.Now().Add(time.Hour).UnixNano())}
		tc := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_PruneTasks, RequestId: 2}
		tc.SetPrune(req)
		h.Handle(conn, encodeTaskControlRequest(t, tc))

		if _, ok := h.Tasks.Get(u); !ok {
			t.Fatal("a confined caller's bare sweep forgot a task outside its scope")
		}
	})
}

func hexOf(t protocol.TaskID) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 32)
	for _, b := range t.Id {
		out = append(out, digits[b>>4], digits[b&0xf])
	}
	return string(out)
}
