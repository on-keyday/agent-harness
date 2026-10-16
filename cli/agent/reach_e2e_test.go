package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

// reachAgent is one agent of the reach e2e: a task record with a creator and
// caps, registered the way an assignment registers it.
type reachAgent struct {
	rid    protocol.RunnerID
	tid    protocol.TaskID
	ticket [16]byte
}

func newReachAgent(t *testing.T, board *agentboard.Board, n byte, creator protocol.TaskID, caps protocol.Capability) reachAgent {
	t.Helper()
	a := reachAgent{rid: mkRidE2E([4]byte{10, 0, 0, n}, 9700+uint16(n), uint16(n)), tid: mkTidE2E(n)}
	a.ticket[0] = n
	v, ok := e2eServers.Load(board)
	if !ok {
		t.Fatal("board was not made by startServerE2E")
	}
	e2eRecord(t, v.(*server.Server), a.tid, creator, caps)
	board.RegisterTask(a.rid, a.tid, a.ticket, "claude")
	return a
}

func (a reachAgent) send(ctx context.Context, addr string, args ...string) (string, error) {
	restore := setAgentEnv(addr, a.rid, a.tid, a.ticket)
	defer restore()
	var out bytes.Buffer
	err := agent.Send(ctx, args, nil, &out)
	return out.String(), err
}

// A confined worker reaches its lineage, the operator and whoever spoke to it,
// and nobody else; board_peer opens the rest; reading a peer's topic needs
// board_observe, except for your own rows on it (D15).
func TestReachE2E_ConfinedWorker(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	sup := newReachAgent(t, board, 0x21, protocol.TaskID{}, 0)
	worker := newReachAgent(t, board, 0x22, sup.tid, 0)
	peer := newReachAgent(t, board, 0x23, protocol.TaskID{}, 0)
	helper := newReachAgent(t, board, 0x24, protocol.TaskID{}, protocol.Capability_BoardPeer)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := worker.send(ctx, addr, "--topic", agentboard.SelfTopic(sup.tid), "--data", "to sup"); err != nil {
		t.Errorf("worker → its creator: %v", err)
	}
	if _, err := sup.send(ctx, addr, "--topic", agentboard.SelfTopic(worker.tid), "--data", "to worker"); err != nil {
		t.Errorf("supervisor → its child: %v", err)
	}
	if _, err := worker.send(ctx, addr, "--topic", agentboard.SelfTopic(peer.tid), "--data", "to peer"); err == nil ||
		!strings.Contains(err.Error(), "board_peer") {
		t.Errorf("worker → a peer: err = %v, want a refusal naming board_peer", err)
	}
	if _, err := helper.send(ctx, addr, "--topic", agentboard.SelfTopic(worker.tid), "--data", "from helper"); err != nil {
		t.Errorf("board_peer holder → worker: %v", err)
	}

	// The worker reads the helper's message, which makes the helper a contact.
	restore := setAgentEnv(addr, worker.rid, worker.tid, worker.ticket)
	var inbox bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json"}, &inbox); err != nil {
		restore()
		t.Fatalf("worker inbox: %v", err)
	}
	restore()
	var helperSeq uint64
	sc := bufio.NewScanner(&inbox)
	for sc.Scan() {
		var rec struct {
			Seq         uint64 `json:"seq"`
			PayloadText string `json:"payload_text"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.PayloadText == "from helper" {
			helperSeq = rec.Seq
		}
	}
	if helperSeq == 0 {
		t.Fatalf("worker's inbox lacks the helper's message: %s", inbox.String())
	}
	if _, err := worker.send(ctx, addr, "--in-reply-to", itoa(helperSeq), "--data", "thanks"); err != nil {
		t.Errorf("worker replying to a message it received: %v", err)
	}

	restore = setAgentEnv(addr, worker.rid, worker.tid, worker.ticket)
	defer restore()
	if err := agent.Subscribe(ctx, []string{"--topic", agentboard.SelfTopic(peer.tid)}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "board_observe") {
		t.Errorf("worker subscribing to a peer's topic: err = %v, want a refusal naming board_observe", err)
	}
	if err := agent.Wait(ctx, []string{"--topic", agentboard.OperatorTopic, "--timeout", "1s"}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "board_observe") {
		t.Errorf("worker waiting on chat.operator: err = %v, want a refusal naming board_observe", err)
	}
	// D15: on the supervisor's topic the worker sees only its own message.
	var retained bytes.Buffer
	if err := agent.Retained(ctx, []string{"--topic", agentboard.SelfTopic(sup.tid)}, &retained); err != nil {
		t.Fatalf("worker retained on its creator's topic: %v", err)
	}
	rows := strings.Count(strings.TrimSpace(retained.String()), "\n") + 1
	if strings.TrimSpace(retained.String()) == "" || rows != 1 || !strings.Contains(retained.String(), `"from_task":"`+hexOf(worker.tid)+`"`) {
		t.Errorf("worker's retained on chat.<sup> = %q, want exactly its own one row", retained.String())
	}
}

func hexOf(t protocol.TaskID) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 32)
	for _, b := range t.Id {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}
