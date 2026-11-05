package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTaskState_Identity(t *testing.T) {
	var rid protocol.RunnerID
	rid.Id = [16]byte{7}
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
	if gotRid.Id != rid.Id {
		t.Errorf("rid = %x, want %x", gotRid.Id, rid.Id)
	}
}

func TestTaskState_WaitScopedSubscription(t *testing.T) {
	ts := newTaskState()
	if ts.matches("topic/a") {
		t.Fatal("fresh taskState must not match topic/a")
	}
	w := ts.beginWait("topic/a", 0, 0)
	if !ts.matches("topic/a") {
		t.Fatal("a live wait must make its topic match")
	}
	ts.endWait("topic/a", w, nil)
	if ts.matches("topic/a") {
		t.Fatal("the wait ended, so topic/a must stop matching")
	}
}

func TestTaskState_WaitDoesNotRemoveExistingSubscription(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/b")
	ts.endWait("topic/b", ts.beginWait("topic/b", 0, 0), nil)
	if !ts.matches("topic/b") {
		t.Fatal("a wait must not remove a subscription the task already held")
	}
}

func TestTaskState_ConcurrentWaitsRefcount(t *testing.T) {
	ts := newTaskState()
	w1 := ts.beginWait("topic/c", 0, 0)
	w2 := ts.beginWait("topic/c", 0, 0)
	ts.endWait("topic/c", w1, nil)
	if !ts.matches("topic/c") {
		t.Fatal("one of two waits ended; topic/c must still match")
	}
	ts.endWait("topic/c", w2, nil)
	if ts.matches("topic/c") {
		t.Fatal("both waits ended; topic/c must stop matching")
	}
}

func TestTaskState_SnapshotPatternsIncludesLiveWait(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/persistent")
	w := ts.beginWait("topic/waited", 0, 0)
	got := map[string]bool{}
	for _, p := range ts.snapshotPatterns() {
		got[p] = true
	}
	if !got["topic/persistent"] || !got["topic/waited"] {
		t.Fatalf("snapshotPatterns = %v, want both the persistent and the waited topic", got)
	}
	ts.endWait("topic/waited", w, nil)
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
	ts.beginWait("topic/both", 0, 0)
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

// handOff decides, while a publish is not yet visible to any wait, whether a
// live wait will hand the message over; endWait settles the decision once the
// wait knows what it returned. Neither may wake twice or lose the wake.

func msgOn(seq, inReplyTo uint64) RetainedMessage {
	return RetainedMessage{Seq: seq, InReplyTo: inReplyTo, Topic: "topic/w"}
}

func TestTaskState_HandedOffAndTakenIsNoWake(t *testing.T) {
	ts := newTaskState()
	w := ts.beginWait("topic/w", 0, 0)
	m := msgOn(5, 0)
	if !ts.handOff("topic/w", m) {
		t.Fatal("a wait with no filter must take the message over")
	}
	if ts.endWait("topic/w", w, []RetainedMessage{m}) {
		t.Fatal("the wait returned the message; no wake is owed")
	}
}

// The wait ended (timeout, detach) without returning what it was handed: the
// wake the publish withheld is emitted now, not lost.
func TestTaskState_HandedOffButNotTakenOwesTheWake(t *testing.T) {
	ts := newTaskState()
	w := ts.beginWait("topic/w", 0, 0)
	if !ts.handOff("topic/w", msgOn(5, 0)) {
		t.Fatal("a wait with no filter must take the message over")
	}
	if !ts.endWait("topic/w", w, nil) {
		t.Fatal("the wait ended without the message; the withheld wake must be emitted")
	}
}

func TestTaskState_FilteredWaitDoesNotTakeOthersMessages(t *testing.T) {
	ts := newTaskState()
	w := ts.beginWait("topic/w", 0, 42)
	if ts.handOff("topic/w", msgOn(5, 0)) {
		t.Fatal("a wait for replies to 42 will not return an unrelated message; the publish must wake")
	}
	if ts.handOff("topic/w", msgOn(6, 7)) {
		t.Fatal("a wait for replies to 42 will not return a reply to 7; the publish must wake")
	}
	if !ts.handOff("topic/w", msgOn(8, 42)) {
		t.Fatal("a reply to 42 is what the wait returns; the publish must not wake")
	}
	if ts.endWait("topic/w", w, []RetainedMessage{msgOn(8, 42)}) {
		t.Fatal("nothing handed over remains untaken")
	}
}

func TestTaskState_WaitAtACursorDoesNotTakeOlderSeqs(t *testing.T) {
	ts := newTaskState()
	ts.beginWait("topic/w", 10, 0)
	if ts.handOff("topic/w", msgOn(9, 0)) {
		t.Fatal("seq 9 is not above the cursor 10; the wait will not return it")
	}
}

func TestTaskState_HandOffOnlyToWaitsOnThatTopic(t *testing.T) {
	ts := newTaskState()
	ts.beginWait("topic/other", 0, 0)
	if ts.handOff("topic/w", msgOn(5, 0)) {
		t.Fatal("no wait on topic/w; the publish must wake")
	}
}

// Two waits on one (task, topic): a message either of them returns needs no
// wake, and one neither returns is woken for exactly once, by the last to end.
func TestTaskState_OverlappingWaitsSettleOnce(t *testing.T) {
	t.Run("one takes it", func(t *testing.T) {
		ts := newTaskState()
		w1 := ts.beginWait("topic/w", 0, 0)
		w2 := ts.beginWait("topic/w", 0, 0)
		m := msgOn(5, 0)
		ts.handOff("topic/w", m)
		if ts.endWait("topic/w", w1, []RetainedMessage{m}) {
			t.Fatal("w1 returned it")
		}
		if ts.endWait("topic/w", w2, nil) {
			t.Fatal("w1 already returned it; w2 ending without it owes nothing")
		}
	})
	t.Run("neither takes it", func(t *testing.T) {
		ts := newTaskState()
		w1 := ts.beginWait("topic/w", 0, 0)
		w2 := ts.beginWait("topic/w", 0, 0)
		ts.handOff("topic/w", msgOn(5, 0))
		if ts.endWait("topic/w", w1, nil) {
			t.Fatal("w2 may still return it; the wake is not owed yet")
		}
		if !ts.endWait("topic/w", w2, nil) {
			t.Fatal("the last wait ended without it; the wake is owed now")
		}
	})
	t.Run("the later one takes it", func(t *testing.T) {
		ts := newTaskState()
		w1 := ts.beginWait("topic/w", 0, 0)
		w2 := ts.beginWait("topic/w", 0, 0)
		m := msgOn(5, 0)
		ts.handOff("topic/w", m)
		if ts.endWait("topic/w", w1, nil) {
			t.Fatal("w2 may still return it")
		}
		if ts.endWait("topic/w", w2, []RetainedMessage{m}) {
			t.Fatal("w2 returned it")
		}
	})
}
