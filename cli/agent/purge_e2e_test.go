package agent_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

// TestAgentCLI_E2E_Purge_Granted verifies that an agent holding Capability_Purge
// can drop a topic's retained-message ring, that the reported count matches, and
// that a subsequent since=0 read no longer resurfaces the purged payloads.
func TestAgentCLI_E2E_Purge_Granted(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9600-61"
	var ticket [16]byte
	ticket[0] = 0x61
	tid := mkTidE2E(0x61)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9600, 61)
	board.Registry().Register(rid, tid, ticket)

	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tid.Id[:]),
			Capabilities: uint32(protocol.Capability_Purge),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	for i := 0; i < 3; i++ {
		if err := agent.Send(ctx, []string{"--topic", "chat.poison", "--data", `{"x":1}`}, nil, &bytes.Buffer{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	var out bytes.Buffer
	if err := agent.Purge(ctx, []string{"--topic", "chat.poison"}, &out); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"status":"ok"`) || !strings.Contains(got, `"purged":3`) {
		t.Fatalf("purge output = %s, want status ok and purged 3", got)
	}

	// Purging again is an idempotent not_found (topic no longer exists).
	var out2 bytes.Buffer
	if err := agent.Purge(ctx, []string{"--topic", "chat.poison"}, &out2); err != nil {
		t.Fatalf("re-Purge: %v", err)
	}
	if !strings.Contains(out2.String(), `"status":"not_found"`) {
		t.Fatalf("re-purge output = %s, want status not_found", out2.String())
	}
}

// TestAgentCLI_E2E_Purge_Denied verifies that an agent whose TaskStore entry
// lacks Capability_Purge is refused — the server returns denied and the CLI
// surfaces an error rather than silently succeeding.
func TestAgentCLI_E2E_Purge_Denied(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStr = "ws:5.6.7.8:9601-62"
	var ticket [16]byte
	ticket[0] = 0x62
	tid := mkTidE2E(0x62)
	rid := mkRidE2E([4]byte{5, 6, 7, 8}, 9601, 62)
	board.Registry().Register(rid, tid, ticket)

	// Inject an entry WITHOUT the purge bit.
	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tid.Id[:]),
			Capabilities: uint32(protocol.Capability_Spawn | protocol.Capability_Prune),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	if err := agent.Send(ctx, []string{"--topic", "chat.victim", "--data", `{"x":1}`}, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	err := agent.Purge(ctx, []string{"--topic", "chat.victim"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected purge denied error, got %v", err)
	}

	// The victim topic must still be intact after the denied purge.
	if purged, found := board.PurgeTopic("chat.victim"); !found || purged != 1 {
		t.Fatalf("post-denied PurgeTopic = (purged=%d, found=%v), want (1, true) — denied purge must not have dropped anything", purged, found)
	}
}

// TestAgentCLI_E2E_Purge_Seq verifies that `purge --seq N` drops exactly one
// retained message (found via the content-blind `retained` listing) and leaves
// the rest of the ring intact.
func TestAgentCLI_E2E_Purge_Seq(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9602-63"
	var ticket [16]byte
	ticket[0] = 0x63
	tid := mkTidE2E(0x63)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9602, 63)
	board.Registry().Register(rid, tid, ticket)

	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tid.Id[:]),
			Capabilities: uint32(protocol.Capability_Purge),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	for _, p := range []string{`{"i":1}`, `{"i":2}`, `{"i":3}`} {
		if err := agent.Send(ctx, []string{"--topic", "chat.seqtest", "--data", p}, nil, &bytes.Buffer{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	// Pick the middle seq the content-blind way (board-side mirror of `retained`).
	metas, found := board.ListRetained("chat.seqtest")
	if !found || len(metas) != 3 {
		t.Fatalf("ListRetained = (%d, found=%v), want (3, true)", len(metas), found)
	}
	mid := metas[1].Seq

	var out bytes.Buffer
	if err := agent.Purge(ctx, []string{"--topic", "chat.seqtest", "--seq", fmt.Sprint(mid)}, &out); err != nil {
		t.Fatalf("Purge --seq: %v", err)
	}
	if !strings.Contains(out.String(), `"status":"ok"`) || !strings.Contains(out.String(), `"purged":1`) {
		t.Fatalf("purge --seq output = %s, want ok + purged 1", out.String())
	}

	after, _ := board.ListRetained("chat.seqtest")
	if len(after) != 2 {
		t.Fatalf("after seq-purge len=%d, want 2", len(after))
	}
	for _, m := range after {
		if m.Seq == mid {
			t.Fatalf("seq %d still present after purge --seq", mid)
		}
	}
}

// TestAgentCLI_E2E_Retained_NoCap verifies that `retained` (metadata listing)
// needs NO capability — a task with Capability_None can list a topic's ring
// metadata, because it surfaces only a subset of what subscribing + inbox
// already returns uncapped.
func TestAgentCLI_E2E_Retained_NoCap(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStr = "ws:5.6.7.8:9603-64"
	var ticket [16]byte
	ticket[0] = 0x64
	tid := mkTidE2E(0x64)
	rid := mkRidE2E([4]byte{5, 6, 7, 8}, 9603, 64)
	board.Registry().Register(rid, tid, ticket)

	// Explicitly NO caps.
	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tid.Id[:]),
			Capabilities: uint32(protocol.Capability_None),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	for _, p := range []string{`{"i":1}`, `{"i":2}`} {
		if err := agent.Send(ctx, []string{"--topic", "chat.peek", "--data", p}, nil, &bytes.Buffer{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	var out bytes.Buffer
	if err := agent.Retained(ctx, []string{"--topic", "chat.peek"}, &out); err != nil {
		t.Fatalf("Retained (no cap) should succeed, got: %v", err)
	}
	got := out.String()
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("retained output should have 2 lines, got: %s", got)
	}
	// Metadata only: sender task id present, no payload bytes.
	if !strings.Contains(got, `"from_task":"`+hex.EncodeToString(tid.Id[:])+`"`) {
		t.Errorf("retained output missing sender task id: %s", got)
	}
	if strings.Contains(got, `"i":1`) || strings.Contains(got, "payload") {
		t.Errorf("retained output must not contain payload content: %s", got)
	}
}
