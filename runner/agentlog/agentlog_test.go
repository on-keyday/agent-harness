package agentlog

import "testing"

func TestPassthroughDecoderEmitsRaw(t *testing.T) {
	d := NewDecoder("")
	evs := d.Decode([]byte("hello world"))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != KindRaw || evs[0].Text != "hello world" {
		t.Fatalf("got %+v, want KindRaw/\"hello world\"", evs[0])
	}
}

func TestUnknownFormatResolvesToPassthrough(t *testing.T) {
	// A misconfigured profile must degrade to today's behaviour, not fail.
	d := NewDecoder("no-such-format")
	evs := d.Decode([]byte(`{"type":"assistant"}`))
	if len(evs) != 1 || evs[0].Kind != KindRaw {
		t.Fatalf("got %+v, want a single KindRaw", evs)
	}
}

func TestKnownFormatsMatchesNewDecoder(t *testing.T) {
	// KnownFormats is the list runner.ProfileSet.UnrecognisedLogFormats
	// validates against; it must name exactly the non-empty strings for
	// which NewDecoder returns something other than passthrough.
	want := []string{"claude-stream-json", "codex-jsonl"}
	got := KnownFormats()
	if len(got) != len(want) {
		t.Fatalf("KnownFormats() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("KnownFormats() = %v, want %v", got, want)
		}
	}

	for _, name := range got {
		if _, ok := NewDecoder(name).(passthrough); ok {
			t.Fatalf("KnownFormats() lists %q, but NewDecoder(%q) resolved to passthrough", name, name)
		}
	}
	if _, ok := NewDecoder("not-a-real-format").(passthrough); !ok {
		t.Fatal("NewDecoder of a name absent from KnownFormats() must still resolve to passthrough")
	}
}

func TestRender(t *testing.T) {
	exit0 := 0
	exit2 := 2
	for _, tc := range []struct {
		name string
		ev   Event
		want string
	}{
		{"raw", Event{Kind: KindRaw, Text: "plain line"}, "plain line"},
		{"session", Event{Kind: KindSessionStart, Text: "sess-123"}, "▶ session sess-123"},
		{"thinking", Event{Kind: KindThinking, Text: "ignored body"}, "· thinking"},
		{"tool start", Event{Kind: KindToolStart, Tool: "Bash", Args: `{"command":"echo one"}`}, `→ Bash: {"command":"echo one"}`},
		{"tool end", Event{Kind: KindToolEnd, Result: "one"}, "← one"},
		{"tool end exit 0", Event{Kind: KindToolEnd, Result: "one", ExitCode: &exit0}, "← one"},
		{"tool end exit 2", Event{Kind: KindToolEnd, Result: "boom", ExitCode: &exit2}, "← boom (exit 2)"},
		{"tool end is_error", Event{Kind: KindToolEnd, Result: "denied", IsError: true}, "← denied [error]"},
		{"text", Event{Kind: KindText, Text: "done"}, "done"},
		{"finish claude", Event{Kind: KindFinish, Stats: Stats{DurationMS: 5365, CostUSD: 0.0163509}}, "✓ 5365ms $0.016351"},
		{"finish codex", Event{Kind: KindFinish, Stats: Stats{InputTokens: 33075, OutputTokens: 168}}, "✓ 33075 in / 168 out"},
		{"finish empty", Event{Kind: KindFinish}, "✓ done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.ev); got != tc.want {
				t.Fatalf("Render() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderTruncatesOnRuneBoundary(t *testing.T) {
	// 150 three-byte runes = 450 bytes; the 200-byte cap must not split one.
	long := ""
	for i := 0; i < 150; i++ {
		long += "あ"
	}
	got := Render(Event{Kind: KindToolEnd, Result: long})
	if len(got) > len("← ")+200+len("…") {
		t.Fatalf("rendered %d bytes, want <= %d", len(got), len("← ")+200+len("…"))
	}
	if !hasSuffix(got, "…") {
		t.Fatalf("got %q, want a trailing ellipsis", got)
	}
	// Splitting a 3-byte rune would leave an invalid UTF-8 tail.
	for _, r := range got {
		if r == '�' {
			t.Fatal("truncation split a multi-byte rune")
		}
	}
}

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
