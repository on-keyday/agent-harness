package server

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/topics"
)

// taskLogTaps owns the per-task pubsub taps that persist task.<id>.log
// traffic into the LogStore — at most ONE tap per task, however many times
// the task is announced. TaskStore.OnCreate deliberately re-fires on every
// Resume (the queued/status events must flow again), but TapSubscribe is
// append-only: registering once per announcement stacked one tap per resume,
// and every later log chunk was appended to the file once per stacked tap.
// A much-resumed task's history then showed each chunk N-fold in every
// operator surface's log pane.
type taskLogTaps struct {
	ps     *pubsub.PubSub
	store  *LogStore
	logger *slog.Logger

	mu   sync.Mutex
	taps map[string]*pubsub.Tap // taskID → registered tap
}

func newTaskLogTaps(ps *pubsub.PubSub, store *LogStore, logger *slog.Logger) *taskLogTaps {
	return &taskLogTaps{ps: ps, store: store, logger: logger, taps: map[string]*pubsub.Tap{}}
}

// Register installs the persistence tap for taskID. Idempotent: a second
// call for the same task (the resume path) is a no-op.
func (t *taskLogTaps) Register(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.taps[taskID]; ok {
		return
	}
	t.taps[taskID] = t.ps.TapSubscribe(topics.TaskLog(taskID), func(_ string, msg []byte) {
		if err := t.store.Append(taskID, msg); err != nil {
			t.logger.Error("logstore append", "task", taskID, "err", err)
		}
	})
}

// Drop removes taskID's tap and releases the LogStore's open handle for it.
// Called on prune, which also deletes the file on disk: without this, a
// straggler publish would resurrect the file, and the store would keep the
// deleted inode's fd open until server shutdown.
func (t *taskLogTaps) Drop(taskID string) {
	t.mu.Lock()
	tap, ok := t.taps[taskID]
	delete(t.taps, taskID)
	t.mu.Unlock()
	if ok {
		t.ps.TapUnsubscribe(topics.TaskLog(taskID), tap)
	}
	t.store.Forget(taskID)
}
