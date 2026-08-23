package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/agent"
)

// dispatch used to wait on a caller-named topic from Since:0 with no
// correlation, so anything already retained there satisfied it — including an
// answer to somebody else's question. It now waits on ITS OWN chat.<short-id>,
// where resolveReplyTarget actually routes a reply, above the seq it published
// and filtered to replies to that seq.
//
// The peer's half is driven through the in-process board rather than a second
// CLI invocation: setAgentEnv mutates process-global HARNESS_* vars, so a
// concurrent second identity would race the dispatcher's own. What is under
// test is dispatch's correlation, not the peer.
func TestAgentCLI_E2E_DispatchIgnoresUnrelatedTraffic(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStrA = "ws:1.2.3.4:9401-41" // the dispatcher
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xD1
	ticketB[0] = 0xD2
	tidA := mkTidE2E(0x41)
	tidB := mkTidE2E(0x42)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9401, 41)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9402, 42)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)

	// Noise that must NOT satisfy the dispatch: an unrelated message sitting on
	// the dispatcher's own topic before it ever asks anything. Under the old
	// Since:0 + no-filter wait this is what came back instead of the answer.
	if _, _, err := board.Send(selfA, []byte(`{"msg":"unrelated-noise"}`),
		ridB, tidB, "peer-host", "", 0); err != nil {
		t.Fatalf("seed noise: %v", err)
	}

	// The peer: wait for the question to land on its own topic, then answer it
	// with in_reply_to set, exactly as `agent send --in-reply-to` would.
	answered := make(chan uint64, 1)
	go func() {
		defer close(answered)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			msgs, found := board.ListRetained(selfB)
			if found && len(msgs) > 0 {
				parent := msgs[0].Seq
				// A reply with no topic routes to the parent sender's own
				// chat.<short-id> — the resolution the server performs for
				// `agent send --in-reply-to` when --topic is omitted.
				_, _, _ = board.Send(agentboard.SelfTopic(msgs[0].FromTask),
					[]byte(`{"msg":"the-answer"}`), ridB, tidB, "peer-host", "", parent)
				answered <- parent
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	defer restoreA()

	var out bytes.Buffer
	if err := agent.Dispatch(ctx,
		[]string{"--topic", selfB, "--data", `{"q":"question"}`, "--timeout", "15s"},
		strings.NewReader(""), &out); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if parent, ok := <-answered; !ok || parent == 0 {
		t.Fatal("the peer never saw the question")
	}

	if strings.Contains(out.String(), "unrelated-noise") {
		t.Fatalf("dispatch = %q, want only the reply to its own publish", out.String())
	}
	if !strings.Contains(out.String(), "the-answer") {
		t.Fatalf("dispatch = %q, want the correlated reply", out.String())
	}
}

// A reply to somebody ELSE's message, on the dispatcher's own topic, must not
// satisfy the wait either: the filter is the seq, not merely "is a reply".
func TestAgentCLI_E2E_DispatchIgnoresReplyToAnotherSeq(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStrA = "ws:1.2.3.4:9403-43"
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xD3
	ticketB[0] = 0xD4
	tidA := mkTidE2E(0x43)
	tidB := mkTidE2E(0x44)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9403, 43)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9404, 44)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)

	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			msgs, found := board.ListRetained(selfB)
			if found && len(msgs) > 0 {
				parent := msgs[0].Seq
				// A decoy: a real reply, on the right topic, to another seq.
				_, _, _ = board.Send(selfA, []byte(`{"msg":"decoy"}`),
					ridB, tidB, "peer-host", "", parent+1000)
				time.Sleep(150 * time.Millisecond)
				_, _, _ = board.Send(selfA, []byte(`{"msg":"the-answer"}`),
					ridB, tidB, "peer-host", "", parent)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	defer restoreA()

	var out bytes.Buffer
	if err := agent.Dispatch(ctx,
		[]string{"--topic", selfB, "--data", `{"q":"question"}`, "--timeout", "15s"},
		strings.NewReader(""), &out); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(out.String(), "decoy") {
		t.Fatalf("dispatch = %q, want the decoy reply to another seq excluded", out.String())
	}
	if !strings.Contains(out.String(), "the-answer") {
		t.Fatalf("dispatch = %q, want the correlated reply", out.String())
	}
}

// --reply-topic is removed rather than ignored: a caller-chosen reply topic
// could only disagree with where the server actually routes the reply.
func TestAgentCLI_E2E_DispatchRejectsReplyTopicFlag(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := agent.Dispatch(ctx,
		[]string{"--topic", "chat.deadbeef", "--reply-topic", "whatever", "--data", "x"},
		strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("--reply-topic must no longer parse")
	}
}

// --timeout must bound the WHOLE call. It used to bound only the server-side
// reply wait, while the publish-acknowledgement wait above it had no deadline
// of its own — so a value the caller set could be exceeded by a phase the flag
// never reached.
//
// The assertion is two-sided on purpose. That it finishes WITHIN the budget is
// the fix; that it does not finish far EARLY is what keeps this from passing
// for the wrong reason (an unrelated failure returning
// immediately would satisfy a one-sided bound too).
func TestAgentCLI_E2E_DispatchTimeoutBoundsTheWholeCall(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9405-45"
	var ticket [16]byte
	ticket[0] = 0xD5
	tid := mkTidE2E(0x45)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9405, 45)
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	const budget = 3 * time.Second
	start := time.Now()
	var out bytes.Buffer
	err := agent.Dispatch(ctx,
		[]string{"--topic", "nobody.answers.this", "--data", "q", "--timeout", budget.String()},
		strings.NewReader(""), &out)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error when nothing replies")
	}
	// The message names what happened; a bare context error would mean the
	// local deadline beat the server's answer back, which is what
	// replyDeadlineMargin exists to prevent.
	if !strings.Contains(err.Error(), "dispatch reply timeout") {
		t.Errorf("err = %v, want it to say 'dispatch reply timeout'", err)
	}
	if elapsed > budget+2*time.Second {
		t.Errorf("took %v, want it bounded by --timeout (%v)", elapsed, budget)
	}
	if elapsed < budget/2 {
		t.Errorf("took only %v — it returned early, so this did not exercise the timeout", elapsed)
	}
}

// dispatch --reply-to declares the destination AND waits there. The peer still
// answers with --in-reply-to alone, so this is the whole feature end to end:
// the script gets its answer and the asking task's own inbox never sees it.
func TestAgentCLI_E2E_DispatchReplyToWaitsWhereItDeclared(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStrA = "ws:1.2.3.4:9505-55"
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC5
	ticketB[0] = 0xC6
	tidA := mkTidE2E(0x55)
	tidB := mkTidE2E(0x56)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9505, 55)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9506, 56)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)
	const replyTo = "rr.dispatch-1"

	// The peer answers with --in-reply-to only, driven through the in-process
	// board: setAgentEnv is process-global, so a second concurrent CLI identity
	// would race the dispatcher's own. board.Send with a non-zero inReplyTo and
	// the destination resolved the way the server resolves it is exactly what a
	// `send --in-reply-to` from that peer produces.
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			msgs, found := board.ListRetained(selfB)
			if found && len(msgs) > 0 {
				parent := msgs[0]
				dest := parent.ReplyToTopic
				if dest == "" {
					dest = agentboard.SelfTopic(parent.FromTask)
				}
				_, _, _ = board.Send(dest, []byte(`{"a":"dispatched-answer"}`),
					ridB, tidB, "peer-host", "", parent.Seq)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	defer restoreA()

	var out bytes.Buffer
	if err := agent.Dispatch(ctx,
		[]string{"--topic", selfB, "--reply-to", replyTo, "--data", `{"q":"q"}`, "--timeout", "15s"},
		strings.NewReader(""), &out); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(out.String(), "dispatched-answer") {
		t.Fatalf("dispatch = %q, want the answer from the declared topic", out.String())
	}
	if got := topicPayloads(t, board, selfA); strings.Contains(got, "dispatched-answer") {
		t.Errorf("the asking task's own topic holds %q — it was supposed to stay clean", got)
	}
}
