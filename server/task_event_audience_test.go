package server

import (
	"bytes"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/pubsub"
	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// eventStream records every TaskStatusEvent a subscriber is sent, decoded, so
// a test can tell "not delivered" from "delivered redacted".
type eventStream struct {
	*recordingStream
	mu  sync.Mutex
	evs []protocol.TaskStatusEvent
}

func (s *eventStream) AppendData(_ bool, data ...[]byte) error {
	var f pubsubproto.PubSubFrame
	if _, err := f.Decode(bytes.Join(data, nil)); err != nil {
		return err
	}
	var ev protocol.TaskStatusEvent
	if err := ev.DecodeExact(f.Data); err != nil {
		return err
	}
	s.mu.Lock()
	s.evs = append(s.evs, ev)
	s.mu.Unlock()
	return nil
}

func (s *eventStream) take() []protocol.TaskStatusEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.evs
	s.evs = nil
	return out
}

type eventTransport struct {
	recordingTransport
	stream *eventStream
}

func (t *eventTransport) CreateBidirectionalStream() trsf.BidirectionalStream {
	t.stream = &eventStream{recordingStream: &recordingStream{}}
	return t.stream
}

func subscribeTasksStatus(t *testing.T, s *Server, cid string) *eventStream {
	t.Helper()
	tp := &eventTransport{}
	sub := pubsub.NewSubscriber(objproto.MustParseConnectionID(cid), tp)
	if resp := s.pubsub.Subscribe(1, topics.TasksStatus(), "test", sub); uint8(resp.Status) != 0 {
		t.Fatalf("Subscribe(tasks.status) status=%v", resp.Status)
	}
	return tp.stream
}

// tasks.status must deliver to exactly the connections `ls` would show the task
// to (spec 2026-10-19-task-progress D8), with the parent hop's row redacted
// (D14), and task_pruned to everyone (D19).
func TestTaskEvents_FilteredByListVisibility(t *testing.T) {
	s := makeEventServer(t)
	aHex := s.tasks.Create("/r", "a", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")
	aID := hexToTaskID(t, aHex)
	cHex := s.tasks.Create("/r", "c", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, aID, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	kHex := s.tasks.Create("/r", "k", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, aID, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")

	const opCID, aCID, kCID = "ws:127.0.0.1:9901-1", "ws:127.0.0.1:9902-1", "ws:127.0.0.1:9903-1"
	recordAgent(s, aCID, aID)
	recordAgent(s, kCID, hexToTaskID(t, kHex))
	op := subscribeTasksStatus(t, s, opCID)
	a := subscribeTasksStatus(t, s, aCID)
	k := subscribeTasksStatus(t, s, kCID)

	// An event on C: in A's subtree, a sibling of K.
	s.tasks.OnHold(cHex)
	if n := len(op.take()); n != 1 {
		t.Errorf("operator got %d events for C, want 1", n)
	}
	if n := len(a.take()); n != 1 {
		t.Errorf("A (C's creator) got %d events for C, want 1", n)
	}
	if evs := k.take(); len(evs) != 0 {
		t.Errorf("K (C's sibling) got %d events for C, want 0 — its ls does not show C", len(evs))
	}

	// An event on A: K sees A only as its parent hop, so it gets the event.
	// That the progress/title fields arrive emptied is asserted in
	// progress_test.go, once the store has a progress line to empty.
	s.tasks.OnHold(aHex)
	if n := len(op.take()); n != 1 {
		t.Errorf("operator got %d events for A, want 1", n)
	}
	kEvs := k.take()
	if len(kEvs) != 1 {
		t.Fatalf("K got %d events for its parent A, want 1 (redacted)", len(kEvs))
	}
	if kEvs[0].TaskStatus != protocol.TaskStatus_Held {
		t.Errorf("K's parent-hop event status = %v, want Held kept", kEvs[0].TaskStatus)
	}
	a.take()

	// task_pruned reaches everyone: the filter cannot run on a removed entry.
	s.tasks.OnPrune(cHex)
	for name, st := range map[string]*eventStream{"operator": op, "A": a, "K": k} {
		if n := len(st.take()); n != 1 {
			t.Errorf("%s got %d task_pruned events, want 1", name, n)
		}
	}
}
