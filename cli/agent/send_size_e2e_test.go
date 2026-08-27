package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// TestAgentCLI_E2E_SendOkLineReportsBytesAndSource pins the two fields that let
// a sender check WHAT it published without reading the message back.
//
// The publish itself succeeds in every case below, including the one where the
// body is wrong — a shell that ate the pipe, a heredoc that expanded to
// nothing, or `--data -` typed as a positional all end in `status: ok`. The
// count and the source are the whole difference between noticing that on the
// spot and discovering it as a peer's silence.
func TestAgentCLI_E2E_SendOkLineReportsBytesAndSource(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9040-1"
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9040, 1)
	tid := mkTidE2E(0x7A)
	var ticket [16]byte
	ticket[0] = 0x7A
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	cases := []struct {
		name       string
		args       []string
		stdin      string
		wantBytes  int
		wantSource string
	}{
		{
			name:       "--data literal",
			args:       []string{"--topic", "topic/size-e2e", "--data", "hello"},
			wantBytes:  5,
			wantSource: "--data",
		},
		{
			name:       "positional words",
			args:       []string{"--topic", "topic/size-e2e", "hello", "world"},
			wantBytes:  11,
			wantSource: "positional",
		},
		{
			name:       "--data - reads stdin",
			args:       []string{"--topic", "topic/size-e2e", "--data", "-"},
			stdin:      "piped body\n",
			wantBytes:  11,
			wantSource: "stdin",
		},
		{
			// `--data -` misread as a positional. A one-byte publish that
			// otherwise looks exactly like a delivered message.
			name:       "bare - positional publishes one byte",
			args:       []string{"--topic", "topic/size-e2e", "-"},
			stdin:      "must not be read",
			wantBytes:  1,
			wantSource: "positional",
		},
		{
			// An empty stdin is the silent failure the count names: ok, seq
			// allocated, delivered, and nothing in the body.
			name:       "empty stdin publishes zero bytes",
			args:       []string{"--topic", "topic/size-e2e", "--data", "-"},
			stdin:      "",
			wantBytes:  0,
			wantSource: "stdin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := agent.Send(ctx, tc.args, strings.NewReader(tc.stdin), &out); err != nil {
				t.Fatalf("agent.Send: %v", err)
			}
			var rec struct {
				Status string `json:"status"`
				Seq    uint64 `json:"seq"`
				Bytes  int    `json:"bytes"`
				Source string `json:"source"`
			}
			line := strings.TrimSpace(out.String())
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("ok line %q is not JSON: %v", line, err)
			}
			if rec.Status != "ok" {
				t.Fatalf("status = %q, want ok (%s)", rec.Status, line)
			}
			if rec.Bytes != tc.wantBytes {
				t.Errorf("bytes = %d, want %d (%s)", rec.Bytes, tc.wantBytes, line)
			}
			if rec.Source != tc.wantSource {
				t.Errorf("source = %q, want %q (%s)", rec.Source, tc.wantSource, line)
			}
		})
	}
}

// TestAgentCLI_E2E_OversizeRejectionNamesTheSize checks that the size survives
// onto the REJECTION too. PayloadTooLarge's only remedy is splitting the body,
// and a sender that piped something in cannot choose a split without knowing
// how big the thing it just tried to publish was.
func TestAgentCLI_E2E_OversizeRejectionNamesTheSize(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr) // MaxPayload: 4096

	const ridStr = "ws:1.2.3.4:9041-1"
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9041, 1)
	tid := mkTidE2E(0x7B)
	var ticket [16]byte
	ticket[0] = 0x7B
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	var out bytes.Buffer
	err := agent.Send(ctx,
		[]string{"--topic", "topic/oversize-size", "--data", strings.Repeat("x", 8192)},
		nil, &out)
	if err == nil {
		t.Fatalf("send of 8192 bytes against MaxPayload 4096 succeeded: %s", out.String())
	}
	if !strings.Contains(err.Error(), "8192 bytes") {
		t.Errorf("err = %q, want it to name the 8192-byte body it refused", err.Error())
	}
	if !strings.Contains(err.Error(), "--data") {
		t.Errorf("err = %q, want it to name where the body came from", err.Error())
	}
}
