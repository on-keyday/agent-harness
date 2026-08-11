package agent_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// TestAgentCLI_E2E_OversizePayloadRejectedAsTooLarge pins the status an
// over-limit send reports. The limit is enforced while the payload stream is
// read, on the read-error path — which otherwise answers bad_frame, an error
// the sender cannot act on. "Your message is too big" has to survive that
// move, because splitting the body is the only remedy the sender has.
func TestAgentCLI_E2E_OversizePayloadRejectedAsTooLarge(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr) // MaxPayload: 4096

	const ridStr = "ws:1.2.3.4:9020-1"
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9020, 1)
	tid := mkTidE2E(0x5A)
	var ticket [16]byte
	ticket[0] = 0x5A
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	var out bytes.Buffer
	err := agent.Send(ctx,
		[]string{"--topic", "topic/oversize", "--data", strings.Repeat("x", 8192)},
		nil,
		&out,
	)

	if err == nil {
		t.Fatalf("send of 8192 bytes against MaxPayload 4096 succeeded: %s", out.String())
	}
	if !strings.Contains(err.Error(), "PayloadTooLarge") {
		t.Errorf("err = %q, want it to name PayloadTooLarge", err.Error())
	}
}

// TestAgentCLI_E2E_PayloadBeyondFlowWindowStillAnswers exercises the case the
// stub in server/agent_payload_bound_test.go cannot: a body larger than
// trsf's 16MB receive window, against a real stream. The server stops reading
// after ~64KiB, so the window closes on a sender that still has most of the
// body queued. That has to end in an answered request rather than a wedge —
// the sender is blocked on a stream nobody will drain, and only the cancel
// unblocks it.
func TestAgentCLI_E2E_PayloadBeyondFlowWindowStillAnswers(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr) // MaxPayload: 4096

	const ridStr = "ws:1.2.3.4:9021-1"
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9021, 1)
	tid := mkTidE2E(0x5B)
	var ticket [16]byte
	ticket[0] = 0x5B
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	// 20MB: comfortably past trsf.InitialFlowWindow (16MB).
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- agent.Send(ctx,
			[]string{"--topic", "topic/huge", "--data", strings.Repeat("x", 20<<20)},
			nil,
			&out,
		)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send of 20MB against MaxPayload 4096 succeeded")
		}
		if !strings.Contains(err.Error(), "PayloadTooLarge") {
			t.Errorf("err = %q, want it to name PayloadTooLarge", err.Error())
		}
	case <-time.After(45 * time.Second):
		t.Fatal("send never returned: the sender is wedged on a stream the server stopped draining")
	}
}

// TestAgentCLI_E2E_DeliveryPastReceiveWindow raises the limit past the point
// where the transport stops absorbing an unread body, and checks a message
// that size survives BOTH legs. Delivery writes the body to its stream before
// sending the response that names the stream, so nothing on the reader's side
// can drain it while it is being written — past the receive window that write
// cannot finish, and the message is accepted on send but never handed over.
//
// The limit exists to be raised; a ceiling the operator discovers as a hang is
// the failure this pins.
func TestAgentCLI_E2E_DeliveryPastReceiveWindow(t *testing.T) {
	const big = 20 << 20 // past trsf.InitialFlowWindow (16MB)

	addr := freePortE2E(t)
	board, _ := startServerE2EWithMaxPayload(t, addr, big+1)

	const (
		ridStrA = "ws:1.2.3.4:9030-1"
		ridStrB = "ws:5.6.7.8:9031-2"
	)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9030, 1)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9031, 2)
	tidA, tidB := mkTidE2E(0x6A), mkTidE2E(0x6B)
	var ticketA, ticketB [16]byte
	ticketA[0], ticketB[0] = 0x6A, 0x6B
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	sendErr := agent.Send(ctx,
		[]string{"--topic", "topic/big", "--data", strings.Repeat("x", big)},
		nil,
		&sendOut,
	)
	restoreA()
	if sendErr != nil {
		t.Fatalf("agent.Send of %d bytes: %v", big, sendErr)
	}

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB()

	done := make(chan error, 1)
	var waitOut bytes.Buffer
	go func() {
		done <- agent.Wait(ctx, []string{"--topic", "topic/big", "--timeout", "30s"}, &waitOut)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent.Wait: %v", err)
		}
		var line struct {
			PayloadB64 string `json:"payload_b64"`
		}
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(waitOut.String())), &line); jerr != nil {
			t.Fatalf("decode wait output: %v", jerr)
		}
		body, derr := base64.StdEncoding.DecodeString(line.PayloadB64)
		if derr != nil {
			t.Fatalf("decode payload_b64: %v", derr)
		}
		if len(body) != big {
			t.Fatalf("delivered %d bytes, want %d", len(body), big)
		}
		if bytes.Count(body, []byte("x")) != big {
			t.Error("delivered body is not the payload that was sent")
		}
	case <-time.After(90 * time.Second):
		t.Fatal("delivery never completed: the body is written to a stream the reader cannot drain yet")
	}
}
