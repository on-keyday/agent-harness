package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
)

func TestEmitMessageLine_InReplyToAlwaysPresent(t *testing.T) {
	var rid agentboard.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid agentboard.TaskID

	for _, tc := range []struct {
		name string
		in   uint64
	}{{"not a reply", 0}, {"reply", 42}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			emitMessageLine(&buf, 7, "t", []byte("hi"), rid, tid, "h", "claude", tc.in)
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			v, ok := rec["in_reply_to"]
			if !ok {
				t.Fatal("in_reply_to absent; it must be emitted unconditionally")
			}
			if uint64(v.(float64)) != tc.in {
				t.Errorf("in_reply_to = %v, want %d", v, tc.in)
			}
		})
	}
}

// mkTestRid builds a RunnerID for emit tests.
func mkTestRid() agentboard.RunnerID {
	var rid agentboard.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	return rid
}

// TestEmitMessageLineForHook_OmitsBodyPastInlineLimit covers the one consumer
// that cannot decline a payload: the hook modes splice their output straight
// into the agent's next prompt, so an inlined body is spent context whether or
// not the agent wanted it. Past the limit the record has to describe the
// message and say how to fetch it, instead of being the message.
func TestEmitMessageLineForHook_OmitsBodyPastInlineLimit(t *testing.T) {
	var buf bytes.Buffer
	payload := bytes.Repeat([]byte("x"), hookInlineLimit+1)
	emitMessageLineForHook(&buf, 500, "chat.abc", payload, mkTestRid(), agentboard.TaskID{}, "h", "claude", 0)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload_b64"]; ok {
		t.Error("payload_b64 present: an over-limit body was inlined into the prompt anyway")
	}
	if _, ok := rec["payload"]; ok {
		t.Error("payload present: an over-limit body was inlined into the prompt anyway")
	}
	if got := rec["payload_bytes"]; got != float64(len(payload)) {
		t.Errorf("payload_bytes = %v, want %d", got, len(payload))
	}
	// The pointer must address THIS message and nothing else. `inbox --since
	// <seq-1>` would also re-deliver every later message, and inbox fetches a
	// whole batch's payloads before emitting any — so a pointer shaped that
	// way pulls bytes it was written to avoid.
	if got, _ := rec["read_with"].(string); !strings.Contains(got, "agent read 500") {
		t.Errorf("read_with = %q, want it to address seq 500 alone", got)
	}
}

// TestEmitMessageLineForHook_InlinesAtTheLimit keeps the guard from moving the
// boundary it was given: everything that fits today must still arrive inline.
func TestEmitMessageLineForHook_InlinesAtTheLimit(t *testing.T) {
	var buf bytes.Buffer
	emitMessageLineForHook(&buf, 7, "t", bytes.Repeat([]byte("x"), hookInlineLimit), mkTestRid(), agentboard.TaskID{}, "h", "claude", 0)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload_b64"]; !ok {
		t.Error("payload_b64 absent at exactly the limit")
	}
	if _, ok := rec["read_with"]; ok {
		t.Error("read_with present: a body that fits needs no fetch instructions")
	}
}

// TestEmitMessageLine_NeverOmitsRegardlessOfSize pins the escape hatch. The
// hook's read_with points at a plain `agent inbox`, so if that path truncated
// too, the pointer would lead nowhere and the body would be unreachable.
func TestEmitMessageLine_NeverOmitsRegardlessOfSize(t *testing.T) {
	var buf bytes.Buffer
	emitMessageLine(&buf, 7, "t", bytes.Repeat([]byte("x"), 4*hookInlineLimit), mkTestRid(), agentboard.TaskID{}, "h", "claude", 0)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload_b64"]; !ok {
		t.Error("payload_b64 absent: the un-truncated read path is what read_with points at")
	}
}
