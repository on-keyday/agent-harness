package cli

import (
	"errors"
	"testing"
)

// conversationFixture builds the shape conversationKey's own comment records
// as measured on a live board: a two-party exchange that is FOUR chains and
// ONE conversation. Each unanswered publish is its own chain; the two reply
// pairs are chains of two. All four key to one conversation because the
// parties are the same throughout.
//
// The rows go through BuildThreads rather than being hand-built, because
// SelectThreads is what stamps Conversation and a hand-built ThreadRow is a
// shape production never produces -- the firing-log records a TUI test that
// failed for exactly that reason.
func conversationFixture() ([]BoardMessage, map[uint64]string) {
	const a = "aaaaaaaa11111111aaaaaaaa11111111"
	const b = "bbbbbbbb22222222bbbbbbbb22222222"
	msgs := []BoardMessage{
		{Seq: 10, FromTaskHex: a, ReceivedAtMs: 1000},                // chain 1 root
		{Seq: 11, InReplyTo: 10, FromTaskHex: b, ReceivedAtMs: 1100}, // chain 1
		{Seq: 12, FromTaskHex: b, ReceivedAtMs: 1200},                // chain 2, unanswered
		{Seq: 13, FromTaskHex: a, ReceivedAtMs: 1300},                // chain 3 root
		{Seq: 14, InReplyTo: 13, FromTaskHex: b, ReceivedAtMs: 1400}, // chain 3
		{Seq: 15, FromTaskHex: a, ReceivedAtMs: 1500},                // chain 4, unanswered
	}
	topicOf := map[uint64]string{
		10: "chat.bbbbbbbb", 11: "chat.aaaaaaaa", 12: "chat.aaaaaaaa",
		13: "chat.bbbbbbbb", 14: "chat.aaaaaaaa", 15: "chat.bbbbbbbb",
	}
	return msgs, topicOf
}

// TestConversationSelectsMoreThanSeq is the whole reason the field exists: on
// ONE fixture, --seq and --conversation must return different row sets. A test
// where both pass does not exercise the gap that motivated the field.
func TestConversationSelectsMoreThanSeq(t *testing.T) {
	msgs, topicOf := conversationFixture()
	rows := BuildThreads(msgs, topicOf)

	bySeq, err := SelectThreads(rows, topicOf, ThreadFilter{Seq: 10})
	if err != nil {
		t.Fatalf("SelectThreads(--seq 10): %v", err)
	}
	if len(bySeq) != 2 {
		t.Fatalf("--seq 10 selected %d rows, want 2 (the chain 10<-11)", len(bySeq))
	}

	key := bySeq[0].Conversation
	if key == "" {
		t.Fatal("selected rows carry no Conversation key; grouping did not stamp them")
	}

	byConv, err := SelectThreads(rows, topicOf, ThreadFilter{Conversation: key})
	if err != nil {
		t.Fatalf("SelectThreads(--conversation %q): %v", key, err)
	}
	if len(byConv) != len(msgs) {
		t.Fatalf("--conversation %q selected %d rows, want all %d: the four chains are one conversation",
			key, len(byConv), len(msgs))
	}
	if len(byConv) == len(bySeq) {
		t.Fatal("--conversation and --seq returned the same count; the fixture does not exercise the gap")
	}
}

// TestConversationUnknownKeyIsAnError: an empty result on a destructive verb
// reads as "already cleared", which is the state the caller is driving toward
// -- so the one wrong conclusion the failure produces is the one they are
// looking for. Matches --seq's SeqNotVisibleError, not --task's empty result.
func TestConversationUnknownKeyIsAnError(t *testing.T) {
	msgs, topicOf := conversationFixture()
	rows := BuildThreads(msgs, topicOf)

	got, err := SelectThreads(rows, topicOf, ThreadFilter{Conversation: "nosuch+key"})
	if got != nil {
		t.Errorf("rows = %d, want nil", len(got))
	}
	var want *ConversationNotVisibleError
	if !errors.As(err, &want) {
		t.Fatalf("err = %v (%T), want *ConversationNotVisibleError", err, err)
	}
	if want.Key != "nosuch+key" {
		t.Errorf("Key = %q, want %q", want.Key, "nosuch+key")
	}
}

// TestConversationComposesWithTask: the axes AND, and a real conversation the
// other axis excludes is an empty result rather than a bad argument -- the
// same distinction SelectThreads already draws for --seq.
func TestConversationComposesWithTask(t *testing.T) {
	msgs, topicOf := conversationFixture()
	rows := BuildThreads(msgs, topicOf)

	all, err := SelectThreads(rows, topicOf, ThreadFilter{})
	if err != nil {
		t.Fatalf("SelectThreads(no filter): %v", err)
	}
	key := all[0].Conversation

	got, err := SelectThreads(rows, topicOf, ThreadFilter{
		Conversation: key,
		Tasks:        []string{"cccccccc33333333cccccccc33333333"},
	})
	if err != nil {
		t.Fatalf("SelectThreads: %v, want an empty result (the conversation exists, the task axis excluded it)", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows = %d, want 0", len(got))
	}
}

// TestConversationKeepsGroupingForEveryOtherFilter guards the refactor that
// moved grouping out of selectChains' three exits into one call: every path
// that used to group must still group.
func TestConversationKeepsGroupingForEveryOtherFilter(t *testing.T) {
	msgs, topicOf := conversationFixture()
	rows := BuildThreads(msgs, topicOf)

	for name, f := range map[string]ThreadFilter{
		"no filter": {},
		"--seq":     {Seq: 10},
		"--task":    {Tasks: []string{"aaaaaaaa11111111aaaaaaaa11111111"}},
	} {
		got, err := SelectThreads(rows, topicOf, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) == 0 {
			t.Fatalf("%s: selected nothing", name)
		}
		for _, r := range got {
			if r.Conversation == "" {
				t.Errorf("%s: seq %d carries no Conversation key", name, r.Msg.Seq)
			}
		}
	}
}
