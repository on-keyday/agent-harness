package agent_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// sendAndSeq sends a payload as the given identity and returns the seq the
// board assigned, so a test can address that one message afterwards.
func sendAndSeq(t *testing.T, ctx context.Context, addr, ridStr string, tid [16]byte, ticket [16]byte, topic, data string) uint64 {
	t.Helper()
	var protoTid = mkTidE2E(0)
	protoTid.Id = tid
	restore := setAgentEnv(addr, ridStr, protoTid, ticket)
	defer restore()
	var out bytes.Buffer
	if err := agent.Send(ctx, []string{"--topic", topic, "--data", data}, nil, &out); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var rec struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rec); err != nil {
		t.Fatalf("send output %q: %v", out.String(), err)
	}
	if rec.Seq == 0 {
		t.Fatal("send reported seq 0")
	}
	return rec.Seq
}

// TestAgentCLI_E2E_ReadSeq_ReturnsTheBodyOfOneMessage is the point of the
// command: the hook hands back a seq for a body it declined to inline, and
// that seq has to be redeemable on its own. `inbox --since <seq-1>` is not a
// substitute — it re-reads every later message, and inbox fetches the whole
// batch's payloads before emitting any, so those bytes cross the wire only to
// be discarded.
func TestAgentCLI_E2E_ReadSeq_ReturnsTheBodyOfOneMessage(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9310-41"
		ridStrB = "ws:5.6.7.8:9311-42"
	)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9310, 41)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9311, 42)
	tidA, tidB := mkTidE2E(0x41), mkTidE2E(0x42)
	var ticketA, ticketB [16]byte
	ticketA[0], ticketB[0] = 0xA4, 0xB4
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/read"}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreB()

	seq := sendAndSeq(t, ctx, addr, ridStrA, tidA.Id, ticketA, "topic/read", "hello-read")

	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB2()

	var out bytes.Buffer
	if err := agent.Read(ctx, []string{strconv.FormatUint(seq, 10)}, &out); err != nil {
		t.Fatalf("agent.Read(%d): %v", seq, err)
	}
	var rec struct {
		Seq        uint64 `json:"seq"`
		Topic      string `json:"topic"`
		PayloadB64 string `json:"payload_b64"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rec); err != nil {
		t.Fatalf("read output %q: %v", out.String(), err)
	}
	if rec.Seq != seq {
		t.Errorf("seq = %d, want %d", rec.Seq, seq)
	}
	if rec.Topic != "topic/read" {
		t.Errorf("topic = %q, want topic/read", rec.Topic)
	}
	body, err := base64.StdEncoding.DecodeString(rec.PayloadB64)
	if err != nil {
		t.Fatalf("payload_b64: %v", err)
	}
	if string(body) != "hello-read" {
		t.Errorf("payload = %q, want %q", body, "hello-read")
	}
}

// TestAgentCLI_E2E_ReadSeq_RefusesATopicTheCallerDoesNotSubscribeTo is the
// reason the op is scoped at all. Seqs are board-global and consecutive, so an
// unscoped read by seq would walk every ring without ever naming a topic —
// and needing the NAME is what keeps rings from being browsable (listing them
// is what info_global gates). A caller outside the topic must not be able to
// tell the message apart from one that never existed.
func TestAgentCLI_E2E_ReadSeq_RefusesATopicTheCallerDoesNotSubscribeTo(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9320-51"
		ridStrC = "ws:9.9.9.9:9322-53"
	)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9320, 51)
	ridC := mkRidE2E([4]byte{9, 9, 9, 9}, 9322, 53)
	tidA, tidC := mkTidE2E(0x51), mkTidE2E(0x53)
	var ticketA, ticketC [16]byte
	ticketA[0], ticketC[0] = 0xA5, 0xC5
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridC, tidC, ticketC)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seq := sendAndSeq(t, ctx, addr, ridStrA, tidA.Id, ticketA, "topic/private", "secret-body")

	// C subscribes to something else entirely, so it is a live agent with a
	// subscription set — just not one covering topic/private.
	restoreC := setAgentEnv(addr, ridStrC, tidC, ticketC)
	defer restoreC()
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/elsewhere"}, &subOut); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var out bytes.Buffer
	err := agent.Read(ctx, []string{strconv.FormatUint(seq, 10)}, &out)
	if err == nil {
		t.Fatalf("read of an unsubscribed topic succeeded: %s", out.String())
	}
	if strings.Contains(out.String(), "secret-body") ||
		strings.Contains(out.String(), base64.StdEncoding.EncodeToString([]byte("secret-body"))) {
		t.Error("the body leaked to a caller outside the topic")
	}
}
