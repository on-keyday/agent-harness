package agentboard

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var (
	exclRid  = protocol.RunnerID{Id: [16]byte{0xee}}
	exclTidA = protocol.TaskID{Id: [16]byte{0xa1}}
	exclTidB = protocol.TaskID{Id: [16]byte{0xb2}}
)

// newExcludeBoard returns a board with tasks A and B subscribed to topic, and
// records which tasks the wake callback fired for.
func newExcludeBoard(t *testing.T, topic string) (b *Board, a, bc *ConnState, woken *[]protocol.TaskID) {
	t.Helper()
	b = New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	var got []protocol.TaskID
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) { got = append(got, tid) })
	a = b.Attach(exclRid, exclTidA, "h", "")
	bc = b.Attach(exclRid, exclTidB, "h", "")
	t.Cleanup(func() { b.Detach(a); b.Detach(bc) })
	if err := b.Subscribe(a, topic); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(bc, topic); err != nil {
		t.Fatal(err)
	}
	return b, a, bc, &got
}

func TestExcludeSelf_PublisherNotWokenNorCounted(t *testing.T) {
	b, _, _, woken := newExcludeBoard(t, "grp")
	_, delivered, err := b.Send("grp", []byte("x"), exclRid, exclTidA, "h", "", 0, WithExcludeSelf())
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (the publisher is not counted)", delivered)
	}
	if len(*woken) != 1 || (*woken)[0] != exclTidB {
		t.Fatalf("woken = %v, want only B", *woken)
	}
}

func TestExcludeSelf_HookSkipsOwnRowAndAdvancesMark(t *testing.T) {
	b, a, _, _ := newExcludeBoard(t, "grp")
	if _, _, err := b.Send("grp", []byte("mine"), exclRid, exclTidA, "h", "", 0, WithExcludeSelf()); err != nil {
		t.Fatal(err)
	}
	if got := b.InboxAdvance(a); len(got) != 0 {
		t.Fatalf("advance after own excluded send = %+v, want empty", got)
	}
	if _, _, err := b.Send("grp", []byte("theirs"), exclRid, exclTidB, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	got := b.InboxAdvance(a)
	if len(got) != 1 || string(got[0].Payload) != "theirs" {
		t.Fatalf("advance = %+v, want exactly B's message", got)
	}
}

func TestExcludeSelf_InboxSkipsOwnRowCursorAdvances(t *testing.T) {
	b, a, _, _ := newExcludeBoard(t, "grp")
	seq, _, err := b.Send("grp", []byte("mine"), exclRid, exclTidA, "h", "", 0, WithExcludeSelf())
	if err != nil {
		t.Fatal(err)
	}
	msgs, cursor := b.Inbox(a, 0)
	if len(msgs) != 0 {
		t.Fatalf("inbox = %+v, want own excluded row omitted", msgs)
	}
	if cursor < seq {
		t.Fatalf("cursor = %d, want >= %d", cursor, seq)
	}
}

func TestExcludeSelf_StillReadable(t *testing.T) {
	b, _, bc, _ := newExcludeBoard(t, "grp")
	seq, _, err := b.Send("grp", []byte("mine"), exclRid, exclTidA, "h", "", 0, WithExcludeSelf())
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.ListRetained("grp")
	if len(msgs) != 1 || !msgs[0].ExcludeSelf {
		t.Fatalf("ListRetained = %+v, want the entry with ExcludeSelf", msgs)
	}
	if m, ok := b.Retained(seq); !ok || string(m.Payload) != "mine" {
		t.Fatalf("Retained(%d) = %+v, %v", seq, m, ok)
	}
	if got := b.InboxAdvance(bc); len(got) != 1 {
		t.Fatalf("B's advance = %+v, want A's message", got)
	}
}

func TestExcludeSelf_DefaultStillSelfPings(t *testing.T) {
	b, a, _, woken := newExcludeBoard(t, "grp")
	self := SelfTopic(exclTidA)
	if err := b.Subscribe(a, self); err != nil {
		t.Fatal(err)
	}
	_, delivered, err := b.Send(self, []byte("ping"), exclRid, exclTidA, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 || len(*woken) != 1 || (*woken)[0] != exclTidA {
		t.Fatalf("delivered=%d woken=%v, want the self-ping to reach A", delivered, *woken)
	}
}
