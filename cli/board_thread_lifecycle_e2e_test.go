//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestThreadLifecycle_RetractThenPurge walks the operator's actual workflow:
// retract when the discussion ends so a resumed peer cannot re-read it, export,
// then purge. Two moments in time, and the second has to reach what the first
// produced.
//
// That reachability is the property the whole feature rests on, and until this
// test it was known only by reading: withdrawLocked moves a message out of
// topic.ring into topic.retracted, and removeSeq scans both. A feature whose
// two stages are ordered checks the order rather than trusting it.
//
// The middle assertion is the one that makes "export later" possible at all --
// a withdrawn thread is GONE from every agent path and STILL THERE for the
// operator.
func TestThreadLifecycle_RetractThenPurge(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)

	var taskA, taskB protocol.TaskID
	taskA.Id[0] = 0xaa
	taskB.Id[0] = 0xbb
	const topicA = "chat.aa000000" // taskB writes here; taskA owns it
	const topicB = "chat.bb000000" // taskA writes here; taskB owns it

	// One conversation across two topics: every chain touches both parties, so
	// conversationKey folds them together. Seeded through the Board directly
	// rather than the server handler, so retire-on-reply does not fire and the
	// test decides for itself which message is already withdrawn.
	send := func(topic string, body string, from protocol.TaskID, agent string, inReplyTo uint64) uint64 {
		t.Helper()
		seq, _, err := srv.Board().Send(topic, []byte(body), protocol.RunnerID{}, from, "h", agent, inReplyTo)
		if err != nil {
			t.Fatalf("seed %s: %v", body, err)
		}
		return seq
	}
	seq1 := send(topicB, "one", taskA, "claude", 0)
	seq2 := send(topicA, "two", taskB, "codex", seq1)
	_ = send(topicB, "three", taskA, "claude", seq2)
	_ = send(topicA, "four", taskB, "codex", 0) // its own chain, same conversation

	// One message is ALREADY withdrawn when the operator gets here. That is the
	// ordinary state, not a corner: a reply withdraws the message it answers on
	// the sender's behalf, so a finished exchange arrives half-retracted.
	if _, ok := srv.Board().RetractSeq(seq1, taskA); !ok {
		t.Fatalf("RetractSeq(%d) = false, want the author's own retract to succeed", seq1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	rows, err := cli.CollectThreadsWith(ctx, c, cli.ThreadFilter{})
	if err != nil {
		t.Fatalf("CollectThreadsWith: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("thread holds %d rows, want 4", len(rows))
	}
	key := rows[0].Conversation
	for _, r := range rows {
		if r.Conversation != key {
			t.Fatalf("rows span %q and %q; the fixture is meant to be ONE conversation across two topics",
				key, r.Conversation)
		}
	}

	// ---- stage 1: retract ----------------------------------------------
	res, err := cli.FanoutThread(ctx, c, cli.ThreadRetract, cli.ThreadFilter{Conversation: key})
	if err != nil {
		t.Fatalf("retract-thread: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("retract-thread stopped: %v", res.Err)
	}
	got := res.Counts()
	want := map[string]int{"retracted": 3, "already-withdrawn": 1, "not-found": 0, "failed": 0, "skipped": 0}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("after retract Counts()[%q] = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}

	// Agent-facing: gone from both topics. ListRetained is what every one of
	// those paths reads.
	for _, topic := range []string{topicA, topicB} {
		if live, _ := srv.Board().ListRetained(topic); len(live) != 0 {
			t.Errorf("%s still delivers %d message(s) to agents", topic, len(live))
		}
	}

	// Operator-facing: all four still readable, payloads included. This is what
	// makes "retract now, export later" a workflow rather than a data loss.
	total := 0
	for _, topic := range []string{topicA, topicB} {
		msgs, ok, rerr := c.BoardRead(ctx, topic)
		if rerr != nil || !ok {
			t.Fatalf("BoardRead(%s) = (found=%v, err=%v)", topic, ok, rerr)
		}
		for _, m := range msgs {
			if !m.Retracted {
				t.Errorf("%s seq %d is not marked retracted", topic, m.Seq)
			}
			if len(m.Payload) == 0 {
				t.Errorf("%s seq %d lost its payload to the withdrawal", topic, m.Seq)
			}
		}
		total += len(msgs)
	}
	if total != 4 {
		t.Fatalf("operator can read %d message(s) after retract, want all 4", total)
	}

	// ---- stage 2: purge ------------------------------------------------
	// THE assertion. Every row is withdrawn now, so a purge that skipped
	// withdrawn messages -- the rule that is correct for retract -- would
	// report four not-founds and leave the board exactly as it was.
	res2, err := cli.FanoutThread(ctx, c, cli.ThreadPurge, cli.ThreadFilter{Conversation: key})
	if err != nil {
		t.Fatalf("purge-thread: %v", err)
	}
	if res2.Err != nil {
		t.Fatalf("purge-thread stopped: %v", res2.Err)
	}
	got2 := res2.Counts()
	if got2["purged"] != 4 || got2["not-found"] != 0 || got2["failed"] != 0 || got2["skipped"] != 0 {
		t.Fatalf("after purge Counts() = %v, want purged 4 and nothing else", got2)
	}

	for _, topic := range []string{topicA, topicB} {
		msgs, _, rerr := c.BoardRead(ctx, topic)
		if rerr != nil {
			t.Fatalf("BoardRead(%s): %v", topic, rerr)
		}
		if len(msgs) != 0 {
			t.Errorf("%s still holds %d message(s) for the operator after purge", topic, len(msgs))
		}
	}
}

// TestThreadLifecycle_SelectorIsRequiredEndToEnd pins that the safety rule is
// not merely declared. The parse layer refuses a selector-less call, so no
// amount of wiring below it can turn one into a board-wide destruction.
func TestThreadLifecycle_SelectorIsRequiredEndToEnd(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	var author protocol.TaskID
	author.Id[0] = 0xcc
	if _, _, err := srv.Board().Send("chat.cc000000", []byte("survivor"), protocol.RunnerID{}, author, "h", "claude", 0); err != nil {
		t.Fatal(err)
	}

	for _, sub := range []string{"retract-thread", "purge-thread"} {
		var out bytes.Buffer
		err := cli.RunBoardSubcmd(context.Background(), peerCID, sub, nil, &out)
		if err == nil {
			t.Errorf("board %s with no selector returned nil error; it must refuse", sub)
			continue
		}
		if !strings.Contains(err.Error(), "at least one of") {
			t.Errorf("board %s refused with %q, want the AtLeastOne rule's wording", sub, err)
		}
		if out.Len() != 0 {
			t.Errorf("board %s wrote %q before refusing", sub, out.String())
		}
	}

	// Untouched.
	if live, _ := srv.Board().ListRetained("chat.cc000000"); len(live) != 1 {
		t.Errorf("the refused call still reached the board: %d message(s) left, want 1", len(live))
	}
}
