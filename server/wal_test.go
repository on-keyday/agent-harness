package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestWALAppendAndReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	must(t, w.Write(WALEvent{Type: "task_created", TaskID: "abc", RepoPath: "/r", Prompt: "p"}))
	must(t, w.Write(WALEvent{Type: "task_assigned", TaskID: "abc", RunnerID: "r1", WorktreeDir: "/wt"}))
	must(t, w.Close())

	events, err := ReadWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("len=%d", len(events))
	}
	if events[0].TaskID != "abc" || events[1].WorktreeDir != "/wt" {
		t.Fatalf("got %+v", events)
	}
	// each event has Ts > 0
	for i, ev := range events {
		if ev.Ts == 0 {
			t.Fatalf("event[%d] missing Ts", i)
		}
	}
}

func TestReadWALMissingFile(t *testing.T) {
	events, err := ReadWAL(filepath.Join(t.TempDir(), "nope.log"))
	if err != nil {
		t.Fatal(err)
	}
	if events != nil {
		t.Fatalf("expected nil events, got %v", events)
	}
}

func TestWALReplayRestoresSelectorAndBoundRunner(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "events.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	var hn protocol.Hostname
	hn.SetName([]byte("gmkhost"))
	sel.SetHostname(hn)
	if err := wal.Write(WALEvent{
		Type:          "task_created",
		TaskID:        "abc",
		Ts:            time.Now().UnixNano(),
		RepoPath:      "/x/repo",
		BoundRunnerID: "runner-A",
		Selector:      sel,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	wal.Close() //nolint:errcheck

	events, err := ReadWAL(walPath)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.BoundRunnerID != "runner-A" {
		t.Fatalf("BoundRunnerID=%q want runner-A", e.BoundRunnerID)
	}
	if e.Selector.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Fatalf("Selector.Kind=%v want ByHostname", e.Selector.Kind)
	}
	// Verify the hostname payload survived the round-trip.
	hn2 := e.Selector.Hostname()
	if hn2 == nil || string(hn2.Name) != "gmkhost" {
		t.Fatalf("Hostname lost in round-trip: %+v", hn2)
	}
}

func TestWALConcurrentWrite(t *testing.T) {
	// 4 goroutines, 25 writes each = 100 total. After Close + ReadWAL, count == 100, no JSON errors.
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	w, _ := OpenWAL(path)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				w.Write(WALEvent{Type: "task_created", TaskID: fmt.Sprintf("g%d-%d", id, i)}) //nolint:errcheck
			}
		}(g)
	}
	wg.Wait()
	w.Close() //nolint:errcheck
	events, err := ReadWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 100 {
		t.Fatalf("got %d, want 100", len(events))
	}
}

// WALEvent is copied field-by-field into and out of walEventJSON by hand
// (MarshalJSON / UnmarshalJSON). Nothing makes the compiler notice when a new
// field is added to one struct and forgotten in the other three places — the
// field just round-trips as its zero, exactly the failure toTaskInfo shipped
// twice and that mapper_completeness_test.go now guards against.
//
// This fills every field of WALEvent, round-trips it through JSON, and asserts
// nothing came back zero. Add a field and forget a copy → red.
func TestWALEventJSONRoundTripCopiesEveryField(t *testing.T) {
	ec := int32(7)
	// A RunnerSelector whose kind names an arm but carries no payload panics the
	// encoder ("invalid union type"), so build the arm the way the replay test
	// above does rather than leaving it half-set.
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	var hn protocol.Hostname
	hn.SetName([]byte("gmkhost"))
	sel.SetHostname(hn)
	in := WALEvent{
		Type:          "task_created",
		TaskID:        "00112233445566778899aabbccddeeff",
		RunnerID:      "ws:127.0.0.1:8539-1",
		RepoPath:      "/repo",
		Prompt:        "do it",
		Kind:          uint8(protocol.TaskKind_Interactive),
		OriginKind:    uint8(protocol.ClientKind_Tui),
		ResumedByKind: uint8(protocol.ClientKind_Cli),
		CreatorTaskID: "ffeeddccbbaa99887766554433221100",
		Capabilities:  uint32(protocol.Capability_Spawn),
		ScopeBase:     uint8(protocol.ScopeBase_None),
		ScopeIDs:      []string{"00112233445566778899aabbccddeeff"},
		AgentProfile:  "codex",
		WorktreeDir:   "/wt",
		ExitCode:      &ec,
		DiffInfo:      []byte("diff"),
		BoundRunnerID: "ws:127.0.0.1:8539-1",
		Reason:        "boom",
		ExtraArgs:     []string{"--flag"},
		Ts:            1234567890,
		Selector:      sel,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WALEvent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rv := reflect.ValueOf(got)
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Errorf("WALEvent.%s is ZERO after a JSON round trip — a copy was "+
				"forgotten in walEventJSON, MarshalJSON or UnmarshalJSON", rt.Field(i).Name)
		}
	}
	if got.ScopeBase != uint8(protocol.ScopeBase_None) || len(got.ScopeIDs) != 1 {
		t.Errorf("scope = base %d ids %v, want none + one id", got.ScopeBase, got.ScopeIDs)
	}
}

// A record written before scopes existed has neither key. It must replay as
// subtree — the pre-change behaviour — not as the strictest setting.
func TestLegacyWALRecordReplaysAsSubtree(t *testing.T) {
	var got WALEvent
	if err := json.Unmarshal([]byte(`{"type":"task_caps_changed","task_id":"aa","capabilities":1}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ScopeBase != 0 {
		t.Fatalf("ScopeBase = %d, want 0", got.ScopeBase)
	}
	s := scopeFromWAL(got.ScopeBase, got.ScopeIDs)
	if s.Base != protocol.ScopeBase_Subtree || len(s.IDs) != 0 {
		t.Fatalf("legacy scope = %+v, want subtree with no ids", s)
	}
}
