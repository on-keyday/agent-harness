package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTaskState_Identity(t *testing.T) {
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	rid.Port = 9001
	rid.UniqueNumber = 7
	var tid protocol.TaskID
	tid.Id[0] = 0xCC
	ts := newTaskState()
	ts.setIdentity(rid, tid, "host-x", "codex")

	gotRid, gotTid, gotHost, gotProfile := ts.identity()
	if gotHost != "host-x" {
		t.Errorf("hostname = %q", gotHost)
	}
	if gotProfile != "codex" {
		t.Errorf("agent profile = %q", gotProfile)
	}
	if gotTid.Id != tid.Id {
		t.Errorf("task = %v", gotTid.Id)
	}
	if gotRid.UniqueNumber != 7 {
		t.Errorf("rid.UniqueNumber = %d", gotRid.UniqueNumber)
	}
}

func TestTaskState_WaitScopedSubscription(t *testing.T) {
	ts := newTaskState()
	if ts.matches("topic/a") {
		t.Fatal("fresh taskState must not match topic/a")
	}
	ts.beginWait("topic/a")
	if !ts.matches("topic/a") {
		t.Fatal("a live wait must make its topic match")
	}
	ts.endWait("topic/a")
	if ts.matches("topic/a") {
		t.Fatal("the wait ended, so topic/a must stop matching")
	}
}

func TestTaskState_WaitDoesNotRemoveExistingSubscription(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/b")
	ts.beginWait("topic/b")
	ts.endWait("topic/b")
	if !ts.matches("topic/b") {
		t.Fatal("a wait must not remove a subscription the task already held")
	}
}

func TestTaskState_ConcurrentWaitsRefcount(t *testing.T) {
	ts := newTaskState()
	ts.beginWait("topic/c")
	ts.beginWait("topic/c")
	ts.endWait("topic/c")
	if !ts.matches("topic/c") {
		t.Fatal("one of two waits ended; topic/c must still match")
	}
	ts.endWait("topic/c")
	if ts.matches("topic/c") {
		t.Fatal("both waits ended; topic/c must stop matching")
	}
}

func TestTaskState_SnapshotPatternsIncludesLiveWait(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/persistent")
	ts.beginWait("topic/waited")
	got := map[string]bool{}
	for _, p := range ts.snapshotPatterns() {
		got[p] = true
	}
	if !got["topic/persistent"] || !got["topic/waited"] {
		t.Fatalf("snapshotPatterns = %v, want both the persistent and the waited topic", got)
	}
	ts.endWait("topic/waited")
	got = map[string]bool{}
	for _, p := range ts.snapshotPatterns() {
		got[p] = true
	}
	if got["topic/waited"] {
		t.Fatal("the wait ended; topic/waited must leave snapshotPatterns")
	}
}

// A topic that is both persistently subscribed and under a live wait must
// appear once, not twice: snapshotPatterns feeds Inbox, which would otherwise
// return every retained message on it twice.
func TestTaskState_SnapshotPatternsDoesNotDuplicate(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/both")
	ts.beginWait("topic/both")
	n := 0
	for _, p := range ts.snapshotPatterns() {
		if p == "topic/both" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("topic/both appears %d times in snapshotPatterns, want 1", n)
	}
}
