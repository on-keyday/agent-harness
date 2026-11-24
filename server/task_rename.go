package server

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// taskNameProblem is why a display name may not be stored, "" when it may: at
// most credentialNameMax bytes of UTF-8 with no C0, DEL or C1 control
// character — the name reaches operators' terminals (credential D21). Shared
// by credential issue and session rename so the two cannot drift.
func taskNameProblem(name []byte) string {
	switch {
	case len(name) > credentialNameMax:
		return fmt.Sprintf("a name is at most %d bytes, this one is %d", credentialNameMax, len(name))
	case !utf8.Valid(name):
		return "a name must be UTF-8"
	}
	for _, r := range string(name) {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Sprintf("a name may not hold the control character %U", r)
		}
	}
	return ""
}

// SetName stores a task's display name and records it as task_renamed
// (delegated-regrant R10). false when the task does not exist.
func (s *TaskStore) SetName(id, name string) bool {
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	e.Name = name
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_renamed", TaskID: id, Name: name}); err != nil {
			slog.Error("WAL write failed", "op", "task_renamed", "task_id", id, "err", err)
		}
	}
	s.mu.Unlock()
	return true
}

// handleTaskRename is session rename (delegated-regrant R3). The operator may
// rename any task. A task may rename a strict descendant inside its resume
// scope (regrantReaches, R1) and never itself: the name is the board sender
// label, so a self-rename would let a task pose as another to readers who go
// by the name. Every refusal of the gate is not_found, so the answer is no
// existence oracle.
func (h *TaskHandler) handleTaskRename(conn ConnHandle, requestID uint32, cid objproto.ConnectionID, req *protocol.TaskRenameRequest) {
	respond := func(status protocol.TaskRenameStatus) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_TaskRename, RequestId: requestID}
		resp.SetTaskRename(protocol.TaskRenameResponse{Status: status})
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	targetHex := hex.EncodeToString(req.TaskId.Id[:])
	if _, ok := h.Tasks.Get(targetHex); !ok ||
		(h.lookupPrincipal(cid).Id != ([16]byte{}) && !h.regrantReaches(cid, targetHex)) {
		respond(protocol.TaskRenameStatus_NotFound)
		return
	}
	if why := taskNameProblem(req.Name); why != "" {
		slog.Warn("task_rename refused", "cid", cid, "task", targetHex, "why", why)
		respond(protocol.TaskRenameStatus_BadName)
		return
	}
	if !h.Tasks.SetName(targetHex, string(req.Name)) {
		respond(protocol.TaskRenameStatus_NotFound)
		return
	}
	respond(protocol.TaskRenameStatus_Ok)
	if h.OnChange != nil {
		h.OnChange()
	}
}
