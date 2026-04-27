package server

import (
	"fmt"
	"path/filepath"
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
