package server

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// A resumed task re-fires TaskStore.OnCreate (deliberately — the status
// events must flow again), so the log tap registration behind OnCreate MUST
// be idempotent per task. It wasn't: TapSubscribe is append-only, one tap
// stacked per resume, and every later log chunk was appended to the file
// once per stacked tap — N-fold duplicated history in every operator
// surface's log pane.
func TestTaskLogTapsRegisterIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ps := pubsub.NewPubSub(slog.Default())
	taps := newTaskLogTaps(ps, store, slog.Default())

	taps.Register("abc")
	taps.Register("abc") // resume path re-announces the same task
	taps.Register("abc")

	ps.Publish("runner", topics.TaskLog("abc"), []byte("[out]line\n"))

	got, _ := os.ReadFile(filepath.Join(dir, "abc.log"))
	if string(got) != "[out]line\n" {
		t.Fatalf("chunk must be appended exactly once, got %q", got)
	}
}

// Drop must stop persistence AND release the LogStore's open handle: prune
// deletes the file on disk, and a straggler publish after that must not
// resurrect it (nor keep a deleted inode open until server shutdown).
func TestTaskLogTapsDropStopsPersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ps := pubsub.NewPubSub(slog.Default())
	taps := newTaskLogTaps(ps, store, slog.Default())

	taps.Register("abc")
	ps.Publish("runner", topics.TaskLog("abc"), []byte("before\n"))

	taps.Drop("abc")
	path := filepath.Join(dir, "abc.log")
	if err := os.Remove(path); err != nil { // what prune does
		t.Fatal(err)
	}
	ps.Publish("runner", topics.TaskLog("abc"), []byte("after\n"))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dropped task's log file must not be resurrected (stat err=%v)", err)
	}

	// Re-register after drop must work (a pruned id could in principle be
	// recreated), so Drop must fully forget, not poison, the id.
	taps.Register("abc")
	ps.Publish("runner", topics.TaskLog("abc"), []byte("fresh\n"))
	got, _ := os.ReadFile(path)
	if string(got) != "fresh\n" {
		t.Fatalf("re-registered task must persist again, got %q", got)
	}
}

// End-to-end shape of the original bug: wire a TaskStore the way Server.Run
// does (OnCreate → tap registration), create + finish + resume a task, and
// assert the post-resume log chunk lands in the file exactly once.
func TestResumedTaskLogNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ps := pubsub.NewPubSub(slog.Default())
	taps := newTaskLogTaps(ps, store, slog.Default())

	ts := NewTaskStore()
	ts.OnCreate = func(id string) { taps.Register(id) }

	id := ts.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, 0, Scope{}, "")
	ts.Finish(id, 0, nil)
	if _, err := ts.Resume(id, "again", nil, protocol.RunnerSelector{}, "",
		protocol.ClientKind_Tui, false, 0, false, Scope{}, protocol.TaskKind_Oneshot, ""); err != nil {
		t.Fatal(err)
	}

	ps.Publish("runner", topics.TaskLog(id), []byte("[out]resumed\n"))

	got, _ := os.ReadFile(filepath.Join(dir, id+".log"))
	if string(got) != "[out]resumed\n" {
		t.Fatalf("resume stacked a duplicate tap: file=%q", got)
	}
}
