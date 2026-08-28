package agentboard

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// taskIDFromByte builds a distinct TaskID for author-vs-other tests.
func taskIDFromByte(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}

func newRetractBoard(t *testing.T) *Board {
	t.Helper()
	b := New(Config{RingN: 4, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	return b
}

// TestRetract_LeavesEveryAgentFacingPath is the property the whole feature
// rests on: after an author retracts, no path an agent can reach still returns
// the message. It exercises each of them rather than one representative,
// because the design deliberately relies on the entry MOVING out of the live
// ring instead of on a filter at each call site — if some path ever grew its
// own copy of the ring, this is what catches it.
func TestRetract_LeavesEveryAgentFacingPath(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	if err := b.Subscribe(conn, "t.retract"); err != nil {
		t.Fatal(err)
	}
	seq, _, err := b.Send("t.retract", []byte("stale instruction"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	if topic, ok := b.RetractSeq(seq, author); !ok || topic != "t.retract" {
		t.Fatalf("RetractSeq = (%q, %v), want (t.retract, true)", topic, ok)
	}

	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 0 {
		t.Errorf("Inbox after retract = %d msgs, want 0", len(msgs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if msgs, timedOut, _ := b.Wait(ctx, conn, "t.retract", 0, 0); len(msgs) != 0 || !timedOut {
		t.Errorf("Wait after retract = %d msgs (timedOut=%v), want 0 and a timeout", len(msgs), timedOut)
	}
	if _, ok := b.Retained(seq); ok {
		t.Error("Retained(seq) still resolves a retracted message")
	}
	if msgs, found := b.ListRetained("t.retract"); !found || len(msgs) != 0 {
		t.Errorf("ListRetained = (%d msgs, found=%v), want (0, true)", len(msgs), found)
	}
	if _, _, ok := b.LookupSeq(seq); ok {
		t.Error("LookupSeq still resolves a retracted message")
	}
}

// TestRetract_OperatorStillSees is the other half: the operator view keeps the
// message, with the withdrawal timestamped. An agent retracts in seconds; if
// that also emptied this list there would be no window left to audit it.
func TestRetract_OperatorStillSees(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.audit")
	seq, _, err := b.Send("t.audit", []byte("what was said"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Fatal("RetractSeq failed")
	}

	msgs, found := b.ListRetracted("t.audit")
	if !found || len(msgs) != 1 {
		t.Fatalf("ListRetracted = (%d msgs, found=%v), want (1, true)", len(msgs), found)
	}
	if got := string(msgs[0].Payload); got != "what was said" {
		t.Errorf("payload = %q, want it preserved verbatim", got)
	}
	if msgs[0].RetractedAt.IsZero() || msgs[0].RetractedAt.Before(before.Add(-time.Second)) {
		t.Errorf("RetractedAt = %v, want a stamp at retraction time", msgs[0].RetractedAt)
	}
	if n, _ := b.RetractedCount("t.audit"); n != 1 {
		t.Errorf("RetractedCount = %d, want 1", n)
	}
}

// TestRetract_AuthorshipIsTheGate: there is no capability check, so authorship
// is the only thing standing between a task and another task's messages.
func TestRetract_AuthorshipIsTheGate(t *testing.T) {
	b := newRetractBoard(t)
	author, other := taskIDFromByte(1), taskIDFromByte(2)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.auth")
	seq, _, err := b.Send("t.auth", []byte("not yours"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := b.RetractSeq(seq, other); ok {
		t.Fatal("a non-author retracted somebody else's message")
	}
	if msgs, _ := b.ListRetained("t.auth"); len(msgs) != 1 {
		t.Errorf("live ring = %d msgs after a refused retract, want 1", len(msgs))
	}
	// A zero task id is what an unauthenticated connection carries; it must
	// match no author rather than everything.
	if _, ok := b.RetractSeq(seq, protocol.TaskID{}); ok {
		t.Error("a zero task id retracted a message")
	}
	// The author still can.
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Error("the author could not retract its own message")
	}
}

// TestRetract_DoesNotConsumeLiveCapacity guards the reason withdrawn messages
// live in their own list: if they shared the ring, a task sending and
// retracting in a loop would push OTHER senders' live messages out of a FIFO
// that is only RingN deep — eroding the operator's view at agent speed by a
// different route than deletion.
func TestRetract_DoesNotConsumeLiveCapacity(t *testing.T) {
	b := newRetractBoard(t) // RingN = 4
	author := taskIDFromByte(1)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.cap")

	for i := 0; i < 4; i++ {
		seq, _, err := b.Send("t.cap", []byte("noise"), testRid, author, "test-host", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := b.RetractSeq(seq, author); !ok {
			t.Fatalf("retract %d failed", i)
		}
	}
	keep := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		seq, _, err := b.Send("t.cap", []byte("keep"), testRid, taskIDFromByte(2), "test-host", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		keep = append(keep, seq)
	}

	msgs, _ := b.ListRetained("t.cap")
	if len(msgs) != 4 {
		t.Fatalf("live ring = %d, want the full 4: retracted entries must not occupy it", len(msgs))
	}
	for i, seq := range keep {
		if msgs[i].Seq != seq {
			t.Errorf("live[%d].Seq = %d, want %d — a retract evicted a live message", i, msgs[i].Seq, seq)
		}
	}
}

// TestRetract_WithdrawnListIsBounded: the withdrawn list is FIFO at its own
// cap, so retracting forever cannot grow memory without bound.
func TestRetract_WithdrawnListIsBounded(t *testing.T) {
	b := newRetractBoard(t) // RingN = 4
	author := taskIDFromByte(1)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.bound")

	for i := 0; i < 6; i++ {
		seq, _, err := b.Send("t.bound", []byte("x"), testRid, author, "test-host", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := b.RetractSeq(seq, author); !ok {
			t.Fatalf("retract %d failed", i)
		}
	}
	msgs, _ := b.ListRetracted("t.bound")
	if len(msgs) != 4 {
		t.Fatalf("withdrawn list = %d entries, want it capped at RingN=4", len(msgs))
	}
	// FIFO: the oldest two were dropped.
	if msgs[0].Seq >= msgs[len(msgs)-1].Seq {
		t.Errorf("withdrawn list is not in ascending seq order: %d..%d", msgs[0].Seq, msgs[len(msgs)-1].Seq)
	}
}

// TestRetract_PurgeStillReaches: purge is the only way to make bytes stop
// existing, so retracting must not put a message beyond its reach.
func TestRetract_PurgeStillReaches(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.purge")
	seq, _, err := b.Send("t.purge", []byte("secret"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Fatal("RetractSeq failed")
	}

	removed, found := b.PurgeSeq("t.purge", seq)
	if !found || !removed {
		t.Fatalf("PurgeSeq on a retracted message = (removed=%v, found=%v), want both true", removed, found)
	}
	if msgs, _ := b.ListRetracted("t.purge"); len(msgs) != 0 {
		t.Errorf("withdrawn list = %d after purge, want 0", len(msgs))
	}
}

// TestRetract_SurvivesSubscriberRevoke: a worker's chat.<short-id> is
// subscribed by that one task, so Revoke used to destroy the topic — and with
// it every instruction the worker had retracted — the moment the task
// finished. That is exactly when an operator has most reason to read them, so a
// topic still holding withdrawn messages outlives its last subscriber.
func TestRetract_SurvivesSubscriberRevoke(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	worker := taskIDFromByte(2)
	var rid protocol.RunnerID
	b.RegisterTask(rid, worker, [16]byte{}, "")
	self := SelfTopic(worker)

	seq, _, err := b.Send(self, []byte("spent instruction"), testRid, author, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Fatal("RetractSeq failed")
	}

	b.Revoke(rid, worker) // the worker's task ends

	msgs, found := b.ListRetracted(self)
	if !found || len(msgs) != 1 {
		t.Fatalf("after Revoke: (%d withdrawn, found=%v), want (1, true) — the audit trail must outlive the task", len(msgs), found)
	}
}

// TestRevoke_StillDropsEmptyTopics is the other half: the exemption is narrow.
// A topic with nothing withdrawn in it still follows its last subscriber out,
// exactly as before.
func TestRevoke_StillDropsEmptyTopics(t *testing.T) {
	b := newRetractBoard(t)
	worker := taskIDFromByte(3)
	var rid protocol.RunnerID
	b.RegisterTask(rid, worker, [16]byte{}, "")
	self := SelfTopic(worker)

	if _, _, err := b.Send(self, []byte("read and done"), testRid, taskIDFromByte(1), "h", "", 0); err != nil {
		t.Fatal(err)
	}

	b.Revoke(rid, worker)

	if _, found := b.ListRetained(self); found {
		t.Error("a topic with no withdrawn messages must still be dropped with its last subscriber")
	}
}

// TestRetract_KeptTopicStillTTLEvicts is the other end of the Revoke
// exemption. Revoke runs once, at task end, so nothing re-evaluates "is the
// withdrawn list still non-empty?" afterwards — if the TTL sweep did not take
// these topics they would be a leak with no second chance to collect them.
//
// It does: evictExpiredTopics has no subscriber condition and keys on the
// topic's LAST PUBLISH, which retraction does not touch. So a topic kept alive
// past its last subscriber still dies at the same moment it always would have.
// The exemption blocks the EARLY deletion; it does not extend the lifetime.
func TestRetract_KeptTopicStillTTLEvicts(t *testing.T) {
	b := New(Config{RingN: 4, TopicTTL: 300 * time.Millisecond, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	author := taskIDFromByte(1)
	worker := taskIDFromByte(2)
	var rid protocol.RunnerID
	b.RegisterTask(rid, worker, [16]byte{}, "")
	self := SelfTopic(worker)

	seq, _, err := b.Send(self, []byte("spent"), testRid, author, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Fatal("RetractSeq failed")
	}
	b.Revoke(rid, worker)
	if msgs, found := b.ListRetracted(self); !found || len(msgs) != 1 {
		t.Fatalf("precondition: topic should have survived Revoke, got (%d, %v)", len(msgs), found)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := b.ListRetained(self); !found {
			return // TTL collected it
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a topic kept past its last subscriber never TTL-evicted: that is a leak, not a longer audit window")
}

// TestRetract_EmptiedWithdrawnListLeavesNoLiveTopic: purging the last withdrawn
// message leaves an empty, subscriber-less topic. Revoke has already run and
// will not run again, so this too has to fall to the TTL sweep — the same
// "harmless, it TTL-evicts like any quiet topic" that PurgeSeq already relies
// on.
func TestRetract_EmptiedWithdrawnListLeavesNoLiveTopic(t *testing.T) {
	b := New(Config{RingN: 4, TopicTTL: 300 * time.Millisecond, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	author := taskIDFromByte(1)
	worker := taskIDFromByte(2)
	var rid protocol.RunnerID
	b.RegisterTask(rid, worker, [16]byte{}, "")
	self := SelfTopic(worker)

	seq, _, _ := b.Send(self, []byte("spent"), testRid, author, "h", "", 0)
	b.RetractSeq(seq, author)
	b.Revoke(rid, worker)

	// The operator erases the withdrawn message outright.
	if removed, found := b.PurgeSeq(self, seq); !removed || !found {
		t.Fatalf("PurgeSeq = (%v, %v), want both true", removed, found)
	}
	if n, _ := b.RetractedCount(self); n != 0 {
		t.Fatalf("withdrawn list = %d after purge, want 0", n)
	}
	// The topic must still EXIST here, or the wait below would pass without the
	// TTL having collected anything.
	if _, found := b.ListRetained(self); !found {
		t.Fatal("precondition: PurgeSeq must empty the topic, not delete it")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := b.ListRetained(self); !found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("an emptied topic kept by the Revoke exemption never TTL-evicted")
}

// TestRetract_IsIdempotentAndBlind: a second retract of the same seq answers
// the same "no" as a seq that never existed. The CLI turns both into
// not_found, which is what keeps the status from confirming the existence of
// any seq on any topic the caller cannot name.
func TestRetract_IsIdempotentAndBlind(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.twice")
	seq, _, err := b.Send("t.twice", []byte("once"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Fatal("first retract failed")
	}
	if _, ok := b.RetractSeq(seq, author); ok {
		t.Error("second retract of the same seq reported success")
	}
	if _, ok := b.RetractSeq(seq+9999, author); ok {
		t.Error("retracting a seq that was never published reported success")
	}
	if _, ok := b.RetractSeq(0, author); ok {
		t.Error("seq 0 is never a real message; retract must refuse it")
	}
}

// TestForceRetract_WithdrawsSomebodyElsesMessage is the board_retract path: a
// caller holding Capability_Purge withdraws a message it did not write. The
// authorship check that guards RetractSeq is deliberately absent, so what this
// pins is the rest of the contract — the entry leaves the live ring exactly as
// an author retract does, and the withdrawn copy records which check let it
// through and who called.
func TestForceRetract_WithdrawsSomebodyElsesMessage(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	operator := taskIDFromByte(2)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.force")
	seq, _, err := b.Send("t.force", []byte("inconvenient"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	if retracted, found := b.ForceRetractSeq("t.force", seq, operator); !retracted || !found {
		t.Fatalf("ForceRetractSeq = (%v, %v), want (true, true)", retracted, found)
	}

	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 0 {
		t.Errorf("Inbox after force-retract = %d msgs, want 0", len(msgs))
	}
	if _, ok := b.Retained(seq); ok {
		t.Error("Retained(seq) still resolves a force-retracted message")
	}
	if msgs, found := b.ListRetained("t.force"); !found || len(msgs) != 0 {
		t.Errorf("ListRetained = (%d msgs, found=%v), want (0, true)", len(msgs), found)
	}

	withdrawn, found := b.ListRetracted("t.force")
	if !found || len(withdrawn) != 1 {
		t.Fatalf("ListRetracted = (%d msgs, found=%v), want (1, true)", len(withdrawn), found)
	}
	m := withdrawn[0]
	if string(m.Payload) != "inconvenient" {
		t.Errorf("withdrawn payload = %q, want the original bytes", m.Payload)
	}
	if m.RetractedAt.IsZero() {
		t.Error("withdrawn message has no RetractedAt stamp")
	}
	if m.RetractedBy != protocol.RetractedBy_PurgeCap {
		t.Errorf("RetractedBy = %v, want purge_cap", m.RetractedBy)
	}
	if m.RetractedByTask.Id != operator.Id {
		t.Errorf("RetractedByTask = %x, want the caller %x", m.RetractedByTask.Id, operator.Id)
	}
	if m.FromTask.Id != author.Id {
		t.Errorf("FromTask = %x, want the author %x — the sender must not be overwritten", m.FromTask.Id, author.Id)
	}
}

// TestRetract_RecordsAuthorProvenance is the same two fields on the authorship
// path. Without it, `board read` would report every withdrawal as purge_cap or
// as author by accident of the zero value rather than by measurement.
func TestRetract_RecordsAuthorProvenance(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(7)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.prov")
	seq, _, err := b.Send("t.prov", []byte("mine"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.RetractSeq(seq, author); !ok {
		t.Fatal("author retract failed")
	}
	withdrawn, _ := b.ListRetracted("t.prov")
	if len(withdrawn) != 1 {
		t.Fatalf("ListRetracted = %d msgs, want 1", len(withdrawn))
	}
	if withdrawn[0].RetractedBy != protocol.RetractedBy_Author {
		t.Errorf("RetractedBy = %v, want author", withdrawn[0].RetractedBy)
	}
	if withdrawn[0].RetractedByTask.Id != author.Id {
		t.Errorf("RetractedByTask = %x, want the author %x", withdrawn[0].RetractedByTask.Id, author.Id)
	}
}

// TestForceRetract_OperatorClientHasNoTaskID: an operator surface holds its
// capabilities directly and has no principal task, so it calls with the zero
// id. That must be accepted — RetractSeq refuses a zero id because there it
// would be an author match against nobody, which is a different question.
func TestForceRetract_OperatorClientHasNoTaskID(t *testing.T) {
	b := newRetractBoard(t)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.opclient")
	seq, _, err := b.Send("t.opclient", []byte("x"), testRid, taskIDFromByte(1), "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if retracted, _ := b.ForceRetractSeq("t.opclient", seq, protocol.TaskID{}); !retracted {
		t.Fatal("a zero caller id must be accepted: an operator client has no principal task")
	}
	withdrawn, _ := b.ListRetracted("t.opclient")
	if len(withdrawn) != 1 || withdrawn[0].RetractedByTask.Id != ([16]byte{}) {
		t.Fatalf("want one withdrawn message recorded with a zero caller id, got %d", len(withdrawn))
	}
	if withdrawn[0].RetractedBy != protocol.RetractedBy_PurgeCap {
		t.Error("a zero caller id must still be recorded as purge_cap, not as an author retract")
	}
}

// TestForceRetract_NotFoundCases: unknown topic, unknown seq, an already
// withdrawn seq and seq 0 all answer the same "no". The handler turns every
// one of them into not_found; a status that distinguished them would confirm
// what a topic holds to a caller that only guessed at the seq.
func TestForceRetract_NotFoundCases(t *testing.T) {
	b := newRetractBoard(t)
	author := taskIDFromByte(1)
	operator := taskIDFromByte(2)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.nf")
	seq, _, err := b.Send("t.nf", []byte("x"), testRid, author, "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := b.ForceRetractSeq("t.nosuch", seq, operator); found {
		t.Error("an unknown topic reported found")
	}
	if retracted, _ := b.ForceRetractSeq("t.nf", seq+9999, operator); retracted {
		t.Error("a seq that was never published reported success")
	}
	if retracted, _ := b.ForceRetractSeq("t.nf", 0, operator); retracted {
		t.Error("seq 0 is never a real message; force-retract must refuse it")
	}
	if retracted, _ := b.ForceRetractSeq("t.nf", seq, operator); !retracted {
		t.Fatal("setup: the real seq should retract once")
	}
	if retracted, _ := b.ForceRetractSeq("t.nf", seq, operator); retracted {
		t.Error("a second force-retract of the same seq reported success")
	}
}

// TestForceRetract_PurgeStillReaches mirrors TestRetract_PurgeStillReaches for
// the new path: purge is the only way to make bytes stop existing, so nothing
// force-retract does may put a message beyond its reach.
func TestForceRetract_PurgeStillReaches(t *testing.T) {
	b := newRetractBoard(t)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "t.forcepurge")
	seq, _, err := b.Send("t.forcepurge", []byte("secret"), testRid, taskIDFromByte(1), "test-host", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if retracted, _ := b.ForceRetractSeq("t.forcepurge", seq, taskIDFromByte(2)); !retracted {
		t.Fatal("force-retract failed")
	}
	if removed, found := b.PurgeSeq("t.forcepurge", seq); !removed || !found {
		t.Fatalf("PurgeSeq after force-retract = (%v, %v), want (true, true)", removed, found)
	}
	if withdrawn, _ := b.ListRetracted("t.forcepurge"); len(withdrawn) != 0 {
		t.Errorf("withdrawn list still holds %d messages after purge", len(withdrawn))
	}
}
