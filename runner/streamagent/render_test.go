package streamagent

import (
	"encoding/json"
	"testing"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// The neutral Event must survive agentlog → neutral → agentlog, or this kind's
// log lines silently say less than the oneshot kind's for the same agent
// output. This used to live in the runner package against a hand-written copy
// of the forward mapper, because that mapper was unexported; it tests the real
// pair now.
func TestEventRoundTripsThroughTheNeutralType(t *testing.T) {
	code := 3
	cases := []agentlog.Event{
		{Kind: agentlog.KindText, Text: "hello"},
		{Kind: agentlog.KindUserText, Text: "what changed?"},
		{Kind: agentlog.KindThinking, Text: "hmm"},
		{Kind: agentlog.KindSessionStart, Text: "sess-1"},
		{Kind: agentlog.KindToolStart, Tool: "Bash", Args: `{"command":"ls"}`},
		{Kind: agentlog.KindToolEnd, Tool: "Bash", Result: "ok", ExitCode: &code},
		{Kind: agentlog.KindToolEnd, Tool: "Write", IsError: true},
		{Kind: agentlog.KindError, Text: "boom"},
		{Kind: agentlog.KindError, Text: "notice", Warning: true},
		{Kind: agentlog.KindFinish, Stats: agentlog.Stats{
			DurationMS: 12, CostUSD: 0.5, InputTokens: 7, OutputTokens: 9}},
		{Kind: agentlog.KindRaw, Text: "unparseable"},
	}
	for _, in := range cases {
		b, _ := json.Marshal(FromAgentlog(in))
		var back Event
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("neutral event does not survive JSON: %v", err)
		}
		if got := back.ToAgentlog(); agentlog.Render(got) != agentlog.Render(in) {
			t.Errorf("render drifted for %+v:\n  before %q\n  after  %q",
				in, agentlog.Render(in), agentlog.Render(got))
		}
	}
}

// DisplayOf is what both chats apply, so its per-kind decisions are pinned
// here rather than in either surface.
func TestDisplayOf(t *testing.T) {
	cases := []struct {
		name string
		m    Msg
		want Display
	}{
		{"hello sets the status", Msg{Kind: KindHello, Hello: &Hello{Vendor: "claude", Protocol: 1}},
			Display{SetStatus: true, Status: "attached · claude protocol 1"}},
		{"answer", Msg{Kind: KindEvent, Event: &Event{Kind: EventText, Text: "hi"}},
			Display{Text: "hi", Tone: ToneText}},
		{"user turn", Msg{Kind: KindEvent, Event: &Event{Kind: EventUserText, Text: "q"}},
			Display{Text: agentlog.UserTurnPrefix + "q", Tone: ToneYou}},
		{"thinking drives the status", Msg{Kind: KindEvent, Event: &Event{Kind: EventThinking}},
			Display{Text: "· thinking", Tone: ToneMuted, SetStatus: true, Status: "thinking…"}},
		{"finish clears it and goes idle", Msg{Kind: KindEvent, Event: &Event{Kind: EventFinish}},
			Display{Text: "✓ done", Tone: ToneMuted, SetStatus: true, Idle: true}},
		{"a replayed thinking touches nothing", Msg{Kind: KindEvent, Event: &Event{Kind: EventThinking, Replay: true}},
			Display{Text: ReplayPrefix + "· thinking", Tone: ToneMuted}},
		{"a replayed answer is muted", Msg{Kind: KindEvent, Event: &Event{Kind: EventText, Text: "old", Replay: true}},
			Display{Text: ReplayPrefix + "old", Tone: ToneMuted}},
		{"a warning", Msg{Kind: KindEvent, Event: &Event{Kind: EventError, Text: "w", Warning: true}},
			Display{Text: "⚠ w", Tone: ToneWarn}},
		{"a failed exit", Msg{Kind: KindExit, Exit: &Exit{Code: -1, Err: "boom"}},
			Display{Text: "agent exited: code=-1 err=boom", Tone: ToneErr, SetStatus: true, Status: "session ended", Idle: true}},
		{"a clean exit", Msg{Kind: KindExit, Exit: &Exit{Code: 0}},
			Display{Text: "agent exited: code=0", Tone: ToneMuted, SetStatus: true, Status: "session ended", Idle: true}},
	}
	for _, c := range cases {
		got, ok := DisplayOf(c.m)
		if !ok || got != c.want {
			t.Errorf("%s: got %+v ok=%v, want %+v", c.name, got, ok, c.want)
		}
	}
	if _, ok := DisplayOf(Msg{Kind: KindRequest, Request: &Request{ID: "r"}}); ok {
		t.Error("a request has no display of its own: both chats render its payload")
	}
}
