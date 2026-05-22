package runner

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AddFakeTaskForListenServer injects a task entry into the most recently
// established listen-mode Session. Test-only — used by Phase B integration
// tests to skip the full Submit+Assign flow (which would spawn claude) and
// just register a task_id so the agent-proxy ceremony's HasTask check
// passes.
//
// Returns an error if no listen-mode session exists yet (i.e. server has
// not completed Phase A reverse-dial).
func AddFakeTaskForListenServer(ctx context.Context, taskID protocol.TaskID) error {
	_ = ctx
	sess := lastListenSession.Load()
	if sess == nil {
		return errors.New("no listen session established")
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.tasks == nil {
		sess.tasks = make(map[string]*taskEntry)
	}
	// Minimum fields to satisfy HasTask + safe map deletion. The fake entry
	// will never actually run an agent process; cancel is a no-op so a
	// future delete() doesn't trip a nil-func call from any cleanup path.
	sess.tasks[hex.EncodeToString(taskID.Id[:])] = &taskEntry{
		cancel: func() {},
	}
	return nil
}
