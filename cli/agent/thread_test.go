package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// threadPeer holds one synthetic agent: its runner/task ids, its ticket and
// its self topic. Board.RegisterTask seeds the chat.<id8> subscription (the
// runner does this for a real task at assign time), so an agent's Thread
// view covers its own topic the moment it exists.
type threadPeer struct {
	rid     protocol.RunnerID
	tid     protocol.TaskID
	ticket  [16]byte
	selfTpc string
	hex     string
}

func newThreadPeer(b byte, ip [4]byte, port uint16) threadPeer {
	var p threadPeer
	p.rid = mkRidE2E(ip, port, uint16(b))
	p.tid = mkTidE2E(b)
	p.ticket[0] = b
	p.selfTpc = agentboard.SelfTopic(p.tid)
	const digits = "0123456789abcdef"
	for _, x := range p.tid.Id {
		p.hex += string([]byte{digits[x>>4], digits[x&0x0f]})
	}
	return p
}

func (p threadPeer) env(addr string) func() {
	return setAgentEnv(addr, p.rid, p.tid, p.ticket)
}

// seedThreadExchange publishes a three-message exchange that spans two
// topics: A roots on its own chat topic, B replies (the reply lands on A's
// topic — the no---reply-to default routes it to the parent sender's chat),
// and A counters (landing on B's topic). Both peers' views are then
// asymmetric, which is the property this verb must state on its surface.
func seedThreadExchange(t *testing.T, addr string, board *agentboard.Board, a, b threadPeer) (rootSeq, replySeq, counterSeq uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	restoreA := a.env(addr)
	var out bytes.Buffer
	if err := agent.Send(ctx, []string{"--topic", a.selfTpc, "--data", "root: the question"}, nil, &out); err != nil {
		restoreA()
		t.Fatalf("A send: %v", err)
	}
	restoreA()
	rootSeq = lastSeq(t, out.String())

	// B replies, declaring --no-retire-on-reply: without it, A's counter
	// would auto-retire B's reply (the server retires the parent when it is
	// answered), and the joined chain this test is about would shrink to one
	// visible row before the assertions ran. It learns the seq from the
	// board, the way a real peer does.
	restoreB := b.env(addr)
	out.Reset()
	if err := agent.Send(ctx, []string{"--in-reply-to", itoa(rootSeq), "--no-retire-on-reply", "--data", "reply: a clarifying question"}, nil, &out); err != nil {
		restoreB()
		t.Fatalf("B send: %v", err)
	}
	restoreB()
	replySeq = lastSeq(t, out.String())

	// A counters. The reply routes to B's chat topic, because B declared no
	// --reply-to: the counter lands where B, and only B, can see it.
	restoreA = a.env(addr)
	out.Reset()
	if err := agent.Send(ctx, []string{"--in-reply-to", itoa(replySeq), "--data", "counter: the answer"}, nil, &out); err != nil {
		restoreA()
		t.Fatalf("A counter: %v", err)
	}
	restoreA()
	counterSeq = lastSeq(t, out.String())

	if rootSeq == 0 || replySeq == 0 || counterSeq == 0 {
		t.Fatalf("exchange incomplete: root=%d reply=%d counter=%d", rootSeq, replySeq, counterSeq)
	}
	return rootSeq, replySeq, counterSeq
}

func lastSeq(t *testing.T, sendOut string) uint64 {
	t.Helper()
	// The send record is JSON with "seq":<n>. Parse it rather than regex it.
	i := strings.Index(sendOut, `"seq":`)
	if i < 0 {
		return 0
	}
	rest := sendOut[i+len(`"seq":`):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0
	}
	var n uint64
	for _, c := range rest[:j] {
		n = n*10 + uint64(c-'0')
	}
	return n
}

func runAgentThread(t *testing.T, addr string, p threadPeer, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	restore := p.env(addr)
	defer restore()
	var out bytes.Buffer
	if err := agent.Thread(ctx, args, &out); err != nil {
		t.Fatalf("agent thread %v: %v", args, err)
	}
	return out.String()
}

// The operator face is gated on board_observe; this face is not. A's view is
// its own topic, which holds the root AND B's reply (the reply routed to the
// parent sender's chat), so A sees a joined two-row chain — no orphan.
func TestAgentThread_OwnSideJoined(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	a := newThreadPeer('a', [4]byte{1, 2, 3, 4}, 9100)
	b := newThreadPeer('b', [4]byte{5, 6, 7, 8}, 9101)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")
	board.RegisterTask(b.rid, b.tid, b.ticket, "claude")
	seedThreadExchange(t, addr, board, a, b)

	got := runAgentThread(t, addr, a)
	if !strings.Contains(got, "root: the question") || !strings.Contains(got, "reply: a clarifying question") {
		t.Errorf("A's own side incomplete:\n%s", got)
	}
	if strings.Contains(got, "counter: the answer") {
		t.Errorf("A's view shows a message that lives on B's topic — impossible:\n%s", got)
	}
	if strings.Contains(got, "ORPHAN") {
		t.Errorf("A's chain is complete; an ORPHAN marker is wrong here:\n%s", got)
	}
	if !strings.Contains(got, "topics THIS task subscribes to") {
		t.Errorf("window statement missing:\n%s", got)
	}
}

// B sees only the counter — its parent (B's own reply) lives on A's topic,
// invisible to B. The chain must render as an ORPHAN-rooted fragment: the
// honest answer to what B can see, not a viewer failure.
func TestAgentThread_OrphanFragmentMarked(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	a := newThreadPeer('a', [4]byte{1, 2, 3, 4}, 9110)
	b := newThreadPeer('b', [4]byte{5, 6, 7, 8}, 9111)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")
	board.RegisterTask(b.rid, b.tid, b.ticket, "claude")
	_, _, counterSeq := seedThreadExchange(t, addr, board, a, b)

	got := runAgentThread(t, addr, b)
	if !strings.Contains(got, "counter: the answer") {
		t.Errorf("B's visible message missing:\n%s", got)
	}
	if !strings.Contains(got, "ORPHAN") {
		t.Errorf("the fragment is not marked ORPHAN:\n%s", got)
	}
	if !strings.Contains(got, itoa(counterSeq)) {
		t.Errorf("counter row missing its seq:\n%s", got)
	}
}

// A seq outside what this task can see is an ERROR naming the seq — the
// agent-worded variant of the operator face's distinction.
func TestAgentThread_SeqOutsideVisibleSetIsError(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	a := newThreadPeer('a', [4]byte{1, 2, 3, 4}, 9120)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	restore := a.env(addr)
	defer restore()
	var out bytes.Buffer
	err := agent.Thread(ctx, []string{"--seq", "999999"}, &out)
	if err == nil {
		t.Fatalf("want an error naming the seq, got none; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "999999") || !strings.Contains(err.Error(), "not readable from this task") {
		t.Errorf("error is not the agent-worded, seq-naming one: %v", err)
	}
}

// --task filters by involvement: B's id keeps A's chain (B is in it); an
// unrelated id keeps nothing.
func TestAgentThread_TaskFilter(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	a := newThreadPeer('a', [4]byte{1, 2, 3, 4}, 9130)
	b := newThreadPeer('b', [4]byte{5, 6, 7, 8}, 9131)
	c := newThreadPeer('c', [4]byte{9, 10, 11, 12}, 9132)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")
	board.RegisterTask(b.rid, b.tid, b.ticket, "claude")
	board.RegisterTask(c.rid, c.tid, c.ticket, "claude")
	seedThreadExchange(t, addr, board, a, b)

	got := runAgentThread(t, addr, a, "--task", b.hex)
	if !strings.Contains(got, "reply: a clarifying question") {
		t.Errorf("--task B dropped B's own message:\n%s", got)
	}
	got = runAgentThread(t, addr, a, "--task", c.hex)
	if strings.Contains(got, "root: the question") {
		t.Errorf("an unrelated task id kept the chain:\n%s", got)
	}
	if !strings.Contains(got, "topics THIS task subscribes to") {
		t.Errorf("empty result lost the window line:\n%s", got)
	}
}

// --headers-only drops the bodies but keeps the published sizes from the
// metas — a zero size would claim a zero-byte message was published.
func TestAgentThread_HeadersOnlyKeepsSizes(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	a := newThreadPeer('a', [4]byte{1, 2, 3, 4}, 9140)
	b := newThreadPeer('b', [4]byte{5, 6, 7, 8}, 9141)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")
	board.RegisterTask(b.rid, b.tid, b.ticket, "claude")
	seedThreadExchange(t, addr, board, a, b)

	got := runAgentThread(t, addr, a, "--headers-only")
	if strings.Contains(got, "root: the question") {
		t.Errorf("--headers-only still printed a body:\n%s", got)
	}
	if !strings.Contains(got, "size=18") || !strings.Contains(got, "size=28") {
		t.Errorf("published sizes missing (want 18 and 28):\n%s", got)
	}
}

// --json emits the chain record: placement fields plus the body as b64.
func TestAgentThread_JSON(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	a := newThreadPeer('a', [4]byte{1, 2, 3, 4}, 9150)
	b := newThreadPeer('b', [4]byte{5, 6, 7, 8}, 9151)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")
	board.RegisterTask(b.rid, b.tid, b.ticket, "claude")
	seedThreadExchange(t, addr, board, a, b)

	got := runAgentThread(t, addr, a, "--json")
	for _, want := range []string{`"depth":0`, `"depth":1`, `"orphan":false`, `"payload_b64":`} {
		if !strings.Contains(got, want) {
			t.Errorf("json record missing %s:\n%s", want, got)
		}
	}
}
