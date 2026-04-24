package server

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
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
