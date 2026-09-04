package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleRestoreTasks puts back task records a prune forgot, rebuilt from the
// server's own WAL.
//
// Scope-gated on prune, the same bit and the same target set the destructive
// half takes. That symmetry is the whole argument: an agent may only prune
// within its scope, so an agent that may not restore within its scope cannot
// undo its own mistake, and the accident this verb exists for is exactly the
// one an agent has. It grants no reach either -- the ids it can name are the
// ids it could already have pruned, and everything it does with a restored
// task afterwards passes the ordinary gate.
//
// The target set needs the WAL. childIndex is built from the LIVE store, so a
// forgotten task has no parent link there and would fall out of every subtree
// -- the walk would stop at the hole the prune left. walChildIndex adds the
// creator edges from task_created, which is the only place they survive, and
// the SAME scopeSet policy then decides. A second scope implementation reading
// the WAL is the drift shape server/capabilities.go exists to prevent.
func (h *TaskHandler) handleRestoreTasks(conn ConnHandle, requestID uint32, cid string, req *protocol.RestoreTasksRequest) {
	respond := func(body protocol.RestoreTasksResponse) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RestoreTasks, RequestId: requestID}
		resp.SetRestoreTasks(body)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	// nil allowed = unrestricted (operator, or a global base), as in PruneFn.
	var allowed map[string]bool
	if all, set := h.restoreScope(cid); !all {
		allowed = set
	}

	if req.ListOnly != 0 {
		// The half that makes the other half usable: a restore takes ids, and
		// the ids of forgotten tasks are in a file on the server host.
		if h.RestorableFn == nil {
			slog.Error("TaskHandler: RestorableFn is not wired")
			respond(protocol.RestoreTasksResponse{WalStatus: protocol.RestoreWALStatus_Unreadable})
			return
		}
		var body protocol.RestoreTasksResponse
		var rows []protocol.RestorableTask
		cands, walStatus := h.RestorableFn()
		body.WalStatus = walStatus
		for _, c := range cands {
			if allowed != nil && !allowed[c.TaskID] {
				continue // out of the caller's scope: not listed, not an oracle
			}
			raw, err := hex.DecodeString(c.TaskID)
			if err != nil || len(raw) != taskIDHexLen/2 {
				continue
			}
			var row protocol.RestorableTask
			copy(row.TaskId.Id[:], raw)
			row.PrunedAt = uint64(c.PrunedAt.UnixNano())
			row.CreatedAt = uint64(c.CreatedAt.UnixNano())
			row.SetRepoPath([]byte(c.RepoPath))
			row.SetPrompt([]byte(c.Prompt))
			rows = append(rows, row)
		}
		body.SetCandidates(rows)
		respond(body)
		return
	}

	if h.RestoreFn == nil {
		slog.Error("TaskHandler: RestoreFn is not wired")
		respond(protocol.RestoreTasksResponse{})
		return
	}
	ids := make([]string, 0, req.TaskIdsLen)
	for i := range req.TaskIds {
		ids = append(ids, hex.EncodeToString(req.TaskIds[i].Id[:]))
	}
	// Out-of-scope ids are dropped before the store is touched, and counted as
	// not_in_wal: indistinguishable from an id that was never written, which
	// is the same answer set_caps gives a target it may not see.
	inScope := make([]string, 0, len(ids))
	outOfScope := 0
	for _, id := range ids {
		if allowed != nil && !allowed[id] {
			outOfScope++
			continue
		}
		inScope = append(inScope, id)
	}
	restored, alreadyPresent, notInWAL := h.RestoreFn(inScope)
	slog.Info("restore_tasks", "cid", cid,
		"restored", len(restored), "already_present", len(alreadyPresent), "not_in_wal", len(notInWAL))
	respond(protocol.RestoreTasksResponse{
		Restored:       uint32(len(restored)),
		AlreadyPresent: uint32(len(alreadyPresent)),
		NotInWal:       uint32(len(notInWAL) + outOfScope),
	})
}

// restoreScope is the caller's target set for a restore: the prune scope,
// widened to see the tasks a prune already removed.
func (h *TaskHandler) restoreScope(cid string) (all bool, allowed map[string]bool) {
	if h.RestoreEventsFn == nil {
		// No WAL to widen with. Fall back to the live set rather than to
		// "everything": a restore that could not read the log cannot justify
		// a wider target set than a prune would have had.
		return h.scopeSet(cid, protocol.Capability_Prune)
	}
	return h.scopeSetWith(cid, protocol.Capability_Prune,
		walChildIndex(h.childIndex(), h.RestoreEventsFn()))
}
