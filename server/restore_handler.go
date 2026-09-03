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
// The gate is the caller having NO principal task, the same one set_caps and
// set_parent use and for the same reason: a bit authorising "restore any id"
// is self-amplifying. An agent holding it could bring back a task outside its
// scope and then act on it, and spawn-time intersection can never claw that
// back. It is not a capability because there is no way to grant it narrowly --
// the ids in the WAL are every task the server has ever seen.
//
// Unlike set_caps this cannot be an existence oracle in the other direction
// either: not_in_wal is reported per id, and an id nobody ever submitted and
// an id outside the caller's world both read the same, because the caller has
// to be the operator to get here at all.
func (h *TaskHandler) handleRestoreTasks(conn ConnHandle, requestID uint32, cid string, req *protocol.RestoreTasksRequest) {
	respond := func(restored, alreadyPresent, notInWAL uint32) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RestoreTasks, RequestId: requestID}
		resp.SetRestoreTasks(protocol.RestoreTasksResponse{
			Restored: restored, AlreadyPresent: alreadyPresent, NotInWal: notInWAL,
		})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	if h.lookupPrincipal(cid).Id != ([16]byte{}) {
		slog.Warn("restore_tasks denied: caller is not an operator", "cid", cid)
		respond(0, 0, 0)
		return
	}
	if h.RestoreFn == nil {
		slog.Error("TaskHandler: RestoreFn is not wired")
		respond(0, 0, 0)
		return
	}

	ids := make([]string, 0, req.TaskIdsLen)
	for i := range req.TaskIds {
		ids = append(ids, hex.EncodeToString(req.TaskIds[i].Id[:]))
	}
	restored, alreadyPresent, notInWAL := h.RestoreFn(ids)
	slog.Info("restore_tasks", "cid", cid,
		"restored", len(restored), "already_present", len(alreadyPresent), "not_in_wal", len(notInWAL))
	respond(uint32(len(restored)), uint32(len(alreadyPresent)), uint32(len(notInWAL)))
}
