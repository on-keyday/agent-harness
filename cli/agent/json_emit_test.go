package agent

import (
	"bytes"
	"encoding/json"
	"io"
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
			emitMessageLine(&buf, mkDM(7, "t", rid, tid, "h", "claude", tc.in, ""), []byte("hi"))
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
	emitMessageLineForHook(&buf, mkDM(500, "chat.abc", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), payload)

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
	emitMessageLineForHook(&buf, mkDM(7, "t", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), bytes.Repeat([]byte("x"), hookInlineLimit))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload_text"]; !ok {
		t.Error("payload_text absent at exactly the limit: the body was not inlined")
	}
	if _, ok := rec["read_with"]; ok {
		t.Error("read_with present: a body that fits needs no fetch instructions")
	}
}

// TestEmitMessageRecord_ProseIsReadable pins the field that keeps a non-JSON
// body from reaching a reader as base64 and nothing else. A model given only
// the blob does not decode it — it guesses at it — so a prose message needs a
// rendering it can actually read, on BOTH paths: the hook splices its record
// into a claude prompt, and a runtime without that hook (codex, bash, …) polls
// the plain read for the same bytes.
func TestEmitMessageRecord_ProseIsReadable(t *testing.T) {
	const prose = "指示: X を実装して\nY は触らないこと"
	for _, tc := range []struct {
		name    string
		emit    func(io.Writer, agentboard.DeliveredMessage, []byte)
		wantB64 bool
	}{
		// The plain read is where `read_with` points and where an exact-bytes
		// consumer lands, so it keeps payload_b64 alongside the text.
		{"plain read", emitMessageLine, true},
		// The hook cannot decline what it is handed, so a body it already
		// rendered readably must not also arrive 4/3-inflated as base64.
		{"hook", emitMessageLineForHook, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(&buf, mkDM(7, "chat.abc", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), []byte(prose))

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			if got, _ := rec["payload_text"].(string); got != prose {
				t.Errorf("payload_text = %q, want %q", got, prose)
			}
			if _, ok := rec["payload_b64"]; ok != tc.wantB64 {
				t.Errorf("payload_b64 present = %v, want %v", ok, tc.wantB64)
			}
			// One record per line: an embedded newline must stay escaped.
			if n := bytes.Count(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("\n")); n != 0 {
				t.Errorf("record spans %d extra lines; JSON-Lines framing broken: %s", n, buf.String())
			}
		})
	}
}

// A JSON body already had a readable rendering under "payload"; payload_text
// would only duplicate it, and under the hook the base64 copy is the third
// rendering of one message.
func TestEmitMessageLineForHook_JSONBodyDropsB64(t *testing.T) {
	var buf bytes.Buffer
	emitMessageLineForHook(&buf, mkDM(7, "t", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), []byte(`{"kind":"review"}`))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload"]; !ok {
		t.Error("payload absent: a JSON body must arrive structured")
	}
	if _, ok := rec["payload_b64"]; ok {
		t.Error("payload_b64 present: the body was already embedded raw")
	}
	if _, ok := rec["payload_text"]; ok {
		t.Error("payload_text present: it duplicates the embedded payload")
	}
}

// Bytes that are neither JSON nor UTF-8 have no readable rendering, so base64
// remains the body — dropping it under the hook would emit a record carrying
// no payload at all.
func TestEmitMessageLineForHook_BinaryKeepsB64(t *testing.T) {
	var buf bytes.Buffer
	emitMessageLineForHook(&buf, mkDM(7, "t", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), []byte{0xff, 0xfe, 0x00, 0x01})

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload_b64"]; !ok {
		t.Error("payload_b64 absent: a non-UTF-8 body has nothing else to arrive as")
	}
	if _, ok := rec["payload_text"]; ok {
		t.Error("payload_text present for non-UTF-8 bytes")
	}
}

// TestEmitMessageLine_NeverOmitsRegardlessOfSize pins the escape hatch. The
// hook's read_with points at a plain `agent inbox`, so if that path truncated
// too, the pointer would lead nowhere and the body would be unreachable.
func TestEmitMessageLine_NeverOmitsRegardlessOfSize(t *testing.T) {
	var buf bytes.Buffer
	emitMessageLine(&buf, mkDM(7, "t", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), bytes.Repeat([]byte("x"), 4*hookInlineLimit))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["payload_b64"]; !ok {
		t.Error("payload_b64 absent: the un-truncated read path is what read_with points at")
	}
}

// mkDM builds a DeliveredMessage for the emit tests, which used to pass these
// as positional arguments.
func mkDM(seq uint64, topic string, rid agentboard.RunnerID, tid agentboard.TaskID, host, profile string, inReplyTo uint64, replyTo string) agentboard.DeliveredMessage {
	m := agentboard.DeliveredMessage{Seq: seq, InReplyTo: inReplyTo, FromRunnerId: rid, FromTaskId: tid}
	m.SetTopic([]byte(topic))
	m.SetFromHostname([]byte(host))
	m.SetFromAgentProfile([]byte(profile))
	m.SetReplyToTopic([]byte(replyTo))
	return m
}

// reply_to_topic is present only when the sender declared one. An empty field
// on every ordinary record would say "nothing happened" once per message, and
// a reader checking for the key is checking the thing that matters.
func TestEmitMessageLine_ReplyToTopic(t *testing.T) {
	var withIt, without bytes.Buffer
	emitMessageLine(&withIt, mkDM(7, "t", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, "rr.dec-019"), []byte("hi"))
	emitMessageLine(&without, mkDM(8, "t", mkTestRid(), agentboard.TaskID{}, "h", "claude", 0, ""), []byte("hi"))

	var got map[string]any
	if err := json.Unmarshal(withIt.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["reply_to_topic"] != "rr.dec-019" {
		t.Errorf("reply_to_topic = %v, want rr.dec-019", got["reply_to_topic"])
	}
	var bare map[string]any
	if err := json.Unmarshal(without.Bytes(), &bare); err != nil {
		t.Fatal(err)
	}
	if _, ok := bare["reply_to_topic"]; ok {
		t.Errorf("undeclared sender emitted reply_to_topic: %s", without.String())
	}
}
