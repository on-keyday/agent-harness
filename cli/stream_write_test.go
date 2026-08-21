package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/streamagent"
)

func TestEncodeStreamMsgAppendsTheNewline(t *testing.T) {
	// The newline is not cosmetic: without it the line sits in the adapter's
	// line buffer, invisible, until some later write flushes it — measured on a
	// live session, and the reason this is one function rather than a
	// json.Marshal at each call site.
	got, err := EncodeStreamMsg(streamagent.Msg{
		Kind: streamagent.KindUser,
		User: &streamagent.UserTurn{Text: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte("\n")) {
		t.Fatalf("no trailing newline: %q", got)
	}
	if bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("want exactly one newline, got %q", got)
	}
	// It must round-trip through the ADAPTER's own decoder, not merely be valid
	// JSON: that decoder is what the far side runs.
	back, err := streamagent.DecodeMsg(bytes.TrimSuffix(got, []byte("\n")))
	if err != nil {
		t.Fatalf("the adapter cannot decode what we built: %v", err)
	}
	if back.Kind != streamagent.KindUser || back.User == nil || back.User.Text != "hello" {
		t.Fatalf("round trip changed the message: %+v", back)
	}
}

func TestEncodeStreamMsgFillsTheProtocolVersion(t *testing.T) {
	// Callers build a Msg by naming the kind and its payload; forgetting V
	// would put a v=0 line on the wire, which the adapter has no reason to
	// accept.
	got, err := EncodeStreamMsg(streamagent.Msg{
		Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{},
	})
	if err != nil {
		t.Fatal(err)
	}
	back, err := streamagent.DecodeMsg(bytes.TrimSuffix(got, []byte("\n")))
	if err != nil {
		t.Fatal(err)
	}
	if back.V != streamagent.ProtocolVersion {
		t.Errorf("V = %d, want %d", back.V, streamagent.ProtocolVersion)
	}
}

func TestEncodeStreamMsgKeepsAMultiLineTurnOnOneLine(t *testing.T) {
	// A pasted multi-line turn is ordinary. JSON escapes the newline inside the
	// string, so the framing survives — asserted rather than assumed, because
	// if it did not the turn would arrive as two undecodable fragments.
	got, err := EncodeStreamMsg(streamagent.Msg{
		Kind: streamagent.KindUser,
		User: &streamagent.UserTurn{Text: "one\ntwo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("a multi-line turn broke the framing: %q", got)
	}
	back, err := streamagent.DecodeMsg(bytes.TrimSuffix(got, []byte("\n")))
	if err != nil {
		t.Fatal(err)
	}
	if back.User.Text != "one\ntwo" {
		t.Errorf("text = %q, want the newline preserved inside the field", back.User.Text)
	}
}

func TestStreamApproveRequiresARequestID(t *testing.T) {
	// The id is what makes a stale answer a refusal rather than a misapplied
	// one (design §3), so an approve without one must not reach the wire.
	var c Client
	err := c.StreamApprove(context.Background(), "deadbeef", streamagent.Response{
		Behavior: streamagent.BehaviorAllow,
	}, 0)
	if err == nil {
		t.Fatal("an approve with no request id must be refused")
	}
	if !strings.Contains(err.Error(), "request id") {
		t.Errorf("the error should name what is missing, got %q", err)
	}
}

func TestDecodeStreamLineMarksANonProtocolLine(t *testing.T) {
	// `session send` can lawfully put a non-protocol line on this stream. A
	// follower that DROPS it cannot explain what the adapter does next, so the
	// line survives with Decoded=false rather than becoming an error.
	line, err := decodeStreamLine([]byte(`hello, not json`))
	if err != nil {
		t.Fatalf("a non-protocol line must not be an error: %v", err)
	}
	if line.Decoded {
		t.Error("Decoded should be false")
	}
	if string(line.Raw) != "hello, not json" {
		t.Errorf("Raw = %q, want the original bytes", line.Raw)
	}

	ok, err := decodeStreamLine([]byte(`{"v":1,"kind":"event","event":{"kind":"text","text":"hi"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Decoded || ok.Msg.Kind != streamagent.KindEvent {
		t.Fatalf("a protocol line must decode: %+v", ok)
	}
	if ok.Msg.Event == nil || ok.Msg.Event.Text != "hi" {
		t.Fatalf("payload lost: %+v", ok.Msg)
	}
}

func TestDecodeStreamLineCopiesItsInput(t *testing.T) {
	// The caller's buffer is reused by bufio; a retained Raw that aliases it
	// would mutate under the surface holding it.
	buf := []byte(`not json`)
	line, _ := decodeStreamLine(buf)
	copy(buf, []byte("XXXXXXXX"))
	if string(line.Raw) != "not json" {
		t.Errorf("Raw aliased the caller's buffer: %q", line.Raw)
	}
}
