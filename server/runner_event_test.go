package server

import (
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
)

// captureRunnerEvents taps runners.status and decodes every RunnerStatusEvent
// published to it. Sibling of captureConnEvents in conn_event_test.go — a tap
// stands in for a real wire subscriber.
func captureRunnerEvents(s *Server) func() []protocol.RunnerStatusEvent {
	var mu sync.Mutex
	var events []protocol.RunnerStatusEvent
	s.pubsub.TapSubscribe(topics.RunnersStatus(), func(_ string, msg []byte) {
		var ev protocol.RunnerStatusEvent
		if err := ev.DecodeExact(msg); err != nil {
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	return func() []protocol.RunnerStatusEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]protocol.RunnerStatusEvent, len(events))
		copy(out, events)
		return out
	}
}

// TestRunnerEvents_CarryIdentity pins that both runner lifecycle events carry
// the runner's RunnerID.
//
// It was published as the zero value on both paths, which is why every
// consumer had to answer a runner event with a full List refetch (see the
// TUI's RunnerEventMsg case) — the event named no row it could update.
// The registry is keyed by ConnectionID, so the identity has to be read off
// the entry; a test that only checked "an event was published" passes either
// way, and nothing renders the field, so nothing else would notice a
// regression.
func TestRunnerEvents_CarryIdentity(t *testing.T) {
	s := New(Config{})
	getEvents := captureRunnerEvents(s)

	identity := protocol.RunnerID{Id: [16]uint8{0xa5, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}}
	cid := objproto.MustParseConnectionID("ws:127.0.0.1:9600-1")
	now := time.Now()

	s.registry.Add(&RunnerEntry{
		ID:           cid,
		Identity:     identity,
		Hostname:     "runner-host",
		AllowedRoots: []string{"/"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  now,
		LastSeen:     now,
		Conn:         stubConn{},
	})
	s.registry.Remove(cid)

	events := getEvents()
	if len(events) != 2 {
		t.Fatalf("expected 2 runner events (registered, offline), got %d: %+v", len(events), events)
	}
	want := []protocol.StatusEventKind{
		protocol.StatusEventKind_RunnerRegistered,
		protocol.StatusEventKind_RunnerOffline,
	}
	for i, ev := range events {
		if ev.Kind != want[i] {
			t.Errorf("event %d: kind = %v, want %v", i, ev.Kind, want[i])
		}
		if ev.RunnerId != identity {
			t.Errorf("event %d (%v): runner_id = %x, want %x", i, ev.Kind, ev.RunnerId.Id, identity.Id)
		}
	}
}
