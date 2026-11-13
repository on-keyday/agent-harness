package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type endLog struct {
	mu  sync.Mutex
	got map[string]protocol.EndReason
}

func (l *endLog) end(store *TaskStore) func(DeadlineHit) {
	return func(d DeadlineHit) {
		l.mu.Lock()
		if l.got == nil {
			l.got = map[string]protocol.EndReason{}
		}
		l.got[d.ID] = d.Reason
		l.mu.Unlock()
		store.CancelBackgroundRun(d.ID, d.Reason, d.SwitchedAt)
	}
}

func (l *endLog) of(id string) (protocol.EndReason, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.got[id]
	return r, ok
}

func waitFor2(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeadlineWatcherEndsAtDeadlineAndQueueTimeout(t *testing.T) {
	store := NewTaskStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	var log endLog
	go runDeadlineWatcher(ctx, store, wake, log.end(store))

	// Added after the watcher started, so only a wake can tell it.
	run := createBG(t, store, protocol.TaskKind_Interactive, RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: 300 * time.Millisecond}})
	store.Assign(run, testRunnerID("r1"), "", false)
	queued := createBG(t, store, protocol.TaskKind_Interactive, RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour, QueueTimeout: 200 * time.Millisecond}})
	select {
	case wake <- struct{}{}:
	default:
	}
	waitFor2(t, 3*time.Second, func() bool { _, a := log.of(run); _, b := log.of(queued); return a && b })
	if r, _ := log.of(run); r != protocol.EndReason_Deadline {
		t.Fatalf("run: %v", r)
	}
	if r, _ := log.of(queued); r != protocol.EndReason_QueueTimeout {
		t.Fatalf("queued: %v", r)
	}
}

func TestDeadlineWatcherStartupPassEndsExpiredHeld(t *testing.T) {
	store := NewTaskStore()
	id := createBG(t, store, protocol.TaskKind_Interactive, RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Millisecond}})
	store.Assign(id, testRunnerID("r1"), "", false)
	store.MarkHold(id, testRunnerID("r1").Hex(), "hold1", time.Now().Add(time.Hour).UnixNano())
	time.Sleep(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log endLog
	go runDeadlineWatcher(ctx, store, make(chan struct{}, 1), log.end(store))
	waitFor2(t, 2*time.Second, func() bool { _, ok := log.of(id); return ok })
	if r, _ := log.of(id); r != protocol.EndReason_Deadline {
		t.Fatalf("held: %v", r)
	}
}

func TestDeadlineWatcherIgnoresTerminal(t *testing.T) {
	store := NewTaskStore()
	id := createBG(t, store, protocol.TaskKind_Interactive, RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: 50 * time.Millisecond}})
	store.Assign(id, testRunnerID("r1"), "", false)
	store.MarkFailed(id, "runner_disconnected")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log endLog
	go runDeadlineWatcher(ctx, store, make(chan struct{}, 1), log.end(store))
	time.Sleep(300 * time.Millisecond)
	if _, ok := log.of(id); ok {
		t.Fatal("the watcher ended a failed task")
	}
}
