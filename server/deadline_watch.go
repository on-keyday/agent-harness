package server

import (
	"context"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runDeadlineWatcher ends background runs whose deadline has passed and queued
// background tasks whose queueing period has (A-D8, A-D13). One watcher for
// both, shaped like watchHeldDeadlines — a startup pass, then a sleep to the
// earliest deadline — except that it never exits while ctx lives: wake (sent
// by TaskStore.OnDeadlineSet) tells it a new, possibly earlier, deadline
// exists. Deadlines are absolute times in the WAL, so the startup pass after
// replay ends whatever passed while the server was down, a Held task included.
func runDeadlineWatcher(ctx context.Context, store *TaskStore, wake <-chan struct{}, end func(id string, reason protocol.EndReason)) {
	for {
		due, next := store.BackgroundDeadlines(time.Now().UnixNano())
		for _, d := range due {
			end(d.ID, d.Reason)
		}
		var timer *time.Timer
		var fire <-chan time.Time
		if next != 0 {
			timer = time.NewTimer(max(time.Until(time.Unix(0, next)), 0))
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
