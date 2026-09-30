package streamagent

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// A heartbeat adapter with a hand-driven clock, so the rate is asserted rather
// than slept for.
func progressAdapter(t *testing.T) (*claudeAdapter, *bytes.Buffer, *time.Time) {
	t.Helper()
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	a := &claudeAdapter{w: NewWriter(&out), now: func() time.Time { return now },
		pending: map[string]string{}, interrupts: map[string]struct{}{}}
	return a, &out, &now
}

func readMsgs(t *testing.T, b *bytes.Buffer) []Msg {
	t.Helper()
	var out []Msg
	rd := NewReader(bytes.NewReader(b.Bytes()))
	for {
		m, err := rd.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("unreadable adapter output: %v", err)
		}
		out = append(out, m)
	}
}

func progressOf(msgs []Msg) []Progress {
	var ps []Progress
	for _, m := range msgs {
		if m.Kind == KindProgress && m.Progress != nil {
			ps = append(ps, *m.Progress)
		}
	}
	return ps
}

func TestProgressHeartbeatFollowsTheBlocksAtABoundedRate(t *testing.T) {
	a, out, now := progressAdapter(t)
	feed := func(s string) { a.handleAgentLine([]byte(s)) }
	tick := func(d time.Duration) { *now = now.Add(d) }

	feed(`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"thinking"}}}`)
	feed(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":120}`) // same second: counted, not sent
	tick(1100 * time.Millisecond)
	feed(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":480}`) // a second on: sent
	feed(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"x"}}}`)
	feed(`{"type":"stream_event","event":{"type":"content_block_stop"}}`)
	feed(`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text"}}}`)
	for i := 0; i < 20; i++ { // twenty deltas inside one second: one heartbeat for all of them
		feed(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"héllo"}}}`)
	}
	tick(time.Second)
	feed(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"!"}}}`)
	feed(`{"type":"stream_event","event":{"type":"content_block_stop"}}`)
	tick(5 * time.Second)
	feed(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"late"}}}`) // after stop: nothing
	feed(`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Bash"}}}`)

	msgs := readMsgs(t, out)
	want := []Progress{
		{Phase: PhaseThinking},
		{Phase: PhaseThinking, Tokens: 480},
		{Phase: PhaseText},
		{Phase: PhaseText, Chars: 101}, // 20 × 5 runes (é is one) + 1
		{Phase: PhaseToolInput, Tool: "Bash"},
	}
	got := progressOf(msgs)
	if len(got) != len(want) {
		t.Fatalf("heartbeats:\n  got  %+v\n  want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("heartbeat %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, m := range msgs {
		if m.Kind == KindEvent && m.Event.Kind == EventRaw {
			t.Fatalf("a partial-message line leaked as a raw event: %+v", m.Event)
		}
	}
}

// thinking_tokens before any content_block_start (or without partial messages
// at all) still starts a thinking heartbeat: it is the one liveness signal the
// agent sends either way, and it used to be dropped.
func TestThinkingTokensAloneStartAHeartbeat(t *testing.T) {
	a, out, _ := progressAdapter(t)
	a.handleAgentLine([]byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":64}`))
	got := progressOf(readMsgs(t, out))
	if len(got) != 1 || got[0] != (Progress{Phase: PhaseThinking, Tokens: 64}) {
		t.Fatalf("got %+v", got)
	}
}

func TestProgressDisplaysOnTheStatusLineOnly(t *testing.T) {
	for _, c := range []struct {
		p    Progress
		want string
	}{
		{Progress{Phase: PhaseThinking}, "thinking… 0 tokens"},
		{Progress{Phase: PhaseThinking, Tokens: 1234}, "thinking… 1.2k tokens"},
		{Progress{Phase: PhaseText, Chars: 340}, "writing… 340 chars"},
		{Progress{Phase: PhaseToolInput, Tool: "Bash", Chars: 12500}, "→ Bash: writing input… 12k chars"},
	} {
		d, ok := DisplayOf(Msg{Kind: KindProgress, Progress: &c.p})
		if !ok || d.Text != "" || !d.SetStatus || d.Status != c.want || d.Idle {
			t.Errorf("%+v: got %+v ok=%v, want status %q and no line", c.p, d, ok, c.want)
		}
	}
	if _, ok := RenderText(Msg{Kind: KindProgress, Progress: &Progress{Phase: PhaseText}}); ok {
		t.Error("a heartbeat renders a log line; the task log is a record, not a pulse")
	}
}

// What an OLDER reader does with a kind it does not know is what makes this
// additive: nothing. Pinned so a future default case cannot reopen the flood
// an unknown EVENT kind would cause (a blank raw line per heartbeat).
func TestAnUnknownMessageKindHasNoDisplay(t *testing.T) {
	m := Msg{Kind: "some-future-kind"}
	if _, ok := DisplayOf(m); ok {
		t.Error("DisplayOf shows an unknown kind")
	}
	if _, ok := RenderText(m); ok {
		t.Error("RenderText renders an unknown kind")
	}
}
