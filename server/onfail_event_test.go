package server

import (
	"encoding/hex"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// captureTaskEvents taps a topic for TaskStatusEvent payloads, the same way
// captureConnEvents in conn_event_test.go taps conns.status: a real Server from
// New() so the hook wiring under test is the production one, and a pubsub tap
// instead of a wire subscriber.
//
// Tapping is what makes this test able to fail. onremove_test.go hand-writes
// the OnRemove closure ("Wire OnRemove as server.go should") and so re-states
// the thing it is checking — it verifies the STORE moved and cannot see whether
// anything was published. That is the layer gap this file exists to close.
func captureTaskEvents(s *Server, topic string) func() []protocol.TaskStatusEvent {
	var mu sync.Mutex
	var events []protocol.TaskStatusEvent
	s.pubsub.TapSubscribe(topic, func(_ string, msg []byte) {
		var ev protocol.TaskStatusEvent
		if err := ev.DecodeExact(msg); err != nil {
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	return func() []protocol.TaskStatusEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]protocol.TaskStatusEvent, len(events))
		copy(out, events)
		return out
	}
}

// detachedTask creates a task, assigns it, and detaches it — the state an
// interactive session sits in when no client is attached.
func detachedTask(t *testing.T, s *Server) string {
	t.Helper()
	id := s.tasks.Create("/repo", "work", protocol.TaskKind_Interactive,
		protocol.ClientKind_Unspecified, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	s.tasks.Assign(id, testRunnerID("ws:127.0.0.1:8539-40"), "", false)
	if err := s.tasks.SetDetached(id); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}
	return id
}

// TestMarkFailedPublishesTaskEnded is the Detached → Failed transition a runner
// disconnect drives (server.go failAndRevokeTasksOf). Every other terminal
// transition reaches tasks.status — Finish via OnFinish, Cancel via OnCancel —
// so a client that follows the topic learns the task ended. MarkFailed had no
// hook at all, which left this transition silent: the store said Failed and
// every event-driven consumer went on showing Detached.
func TestMarkFailedPublishesTaskEnded(t *testing.T) {
	s := New(Config{})
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	id := detachedTask(t, s)

	s.tasks.MarkFailed(id, "runner_disconnected")

	var ended []protocol.TaskStatusEvent
	for _, ev := range getEvents() {
		if ev.Kind == protocol.StatusEventKind_TaskEnded {
			ended = append(ended, ev)
		}
	}
	if len(ended) != 1 {
		t.Fatalf("task_ended events = %d, want 1 (MarkFailed must publish like Finish and Cancel do)", len(ended))
	}
	if ended[0].TaskStatus != protocol.TaskStatus_Failed {
		t.Errorf("TaskStatus = %v, want Failed", ended[0].TaskStatus)
	}
	if got := hex.EncodeToString(ended[0].TaskId.Id[:]); got != id {
		t.Errorf("TaskId = %s, want %s", got, id)
	}
}

// TestMarkFailedPublishesToPerTaskTopic pins the second half of
// publishTaskEvent: it writes the global topic AND task.<id>.status, and a
// client watching one task subscribes only to the latter.
func TestMarkFailedPublishesToPerTaskTopic(t *testing.T) {
	s := New(Config{})
	id := detachedTask(t, s)
	getEvents := captureTaskEvents(s, topics.TaskStatus(id))

	s.tasks.MarkFailed(id, "runner_disconnected")

	events := getEvents()
	if len(events) != 1 {
		t.Fatalf("events on %s = %d, want 1", topics.TaskStatus(id), len(events))
	}
	if events[0].TaskStatus != protocol.TaskStatus_Failed {
		t.Errorf("TaskStatus = %v, want Failed", events[0].TaskStatus)
	}
}

// TestMarkFailedIdempotentPublishesOnce guards the other direction: MarkFailed
// skips an already-terminal task, so the second call must publish nothing. A
// hook called before the terminal check would emit a second task_ended and make
// a subscriber's row flap.
func TestMarkFailedIdempotentPublishesOnce(t *testing.T) {
	s := New(Config{})
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	id := detachedTask(t, s)

	s.tasks.MarkFailed(id, "runner_disconnected")
	s.tasks.MarkFailed(id, "runner_disconnected")

	n := 0
	for _, ev := range getEvents() {
		if ev.Kind == protocol.StatusEventKind_TaskEnded {
			n++
		}
	}
	if n != 1 {
		t.Errorf("task_ended events = %d, want 1 (the second MarkFailed is a no-op)", n)
	}
}

// TestMarkFailedOnHeldPublishesNothing pins the Held carve-out. MarkFailed
// refuses a held task — its child is alive on a runner that agreed to keep it —
// so the disconnect path must not announce an end that did not happen.
func TestMarkFailedOnHeldPublishesNothing(t *testing.T) {
	s := New(Config{})
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	id := detachedTask(t, s)
	if err := s.tasks.MarkHold(id, testRunnerID("ws:127.0.0.1:8539-40").Hex(), "abcd", 0); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	s.tasks.MarkFailed(id, "runner_disconnected")

	for _, ev := range getEvents() {
		if ev.Kind == protocol.StatusEventKind_TaskEnded {
			t.Fatalf("a held task must not publish task_ended")
		}
	}
	if got, _ := s.tasks.Get(id); got.Status != protocol.TaskStatus_Held {
		t.Errorf("status = %v, want Held", got.Status)
	}
}
