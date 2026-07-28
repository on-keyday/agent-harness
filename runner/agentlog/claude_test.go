package agentlog

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// decodeFixture runs every line of a testdata file through d and returns the
// rendered lines, which is exactly what the runner publishes.
func decodeFixture(t *testing.T, path string, d Decoder) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		for _, ev := range d.Decode(sc.Bytes()) {
			out = append(out, Render(ev))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return out
}

func TestClaudeDecoderGolden(t *testing.T) {
	got := decodeFixture(t, "testdata/claude-stream-json.jsonl", NewDecoder("claude-stream-json"))
	if len(got) == 0 {
		t.Fatal("decoder produced no lines")
	}
	// The fixture's session id is random per capture, so match its shape here
	// and assert the id itself in TestClaudeDecoderEmitsSessionStart.
	if !strings.HasPrefix(got[0], "▶ session ") {
		t.Fatalf("line 0 = %q, want a session line", got[0])
	}
	want := []string{
		"· thinking",
		`→ Bash: {"command":"echo one","description":"Run echo one"}`,
		"← one",
		"· thinking",
		"done",
		"✓ 5365ms $0.016351",
	}
	rest := got[1:]
	if len(rest) != len(want) {
		t.Fatalf("got %d lines after the session line, want %d:\n%s",
			len(rest), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i+1, rest[i], want[i])
		}
	}
}

func TestClaudeDecoderEmitsSessionStart(t *testing.T) {
	// The init event carries the session id; assert it decodes rather than
	// hard-coding the fixture's random id in the golden list above.
	d := NewDecoder("claude-stream-json")
	evs := d.Decode([]byte(`{"type":"system","subtype":"init","session_id":"abc-123"}`))
	if len(evs) != 1 || evs[0].Kind != KindSessionStart || evs[0].Text != "abc-123" {
		t.Fatalf("got %+v, want one KindSessionStart with the session id", evs)
	}
}

func TestClaudeDecoderMalformedAndUnknown(t *testing.T) {
	d := NewDecoder("claude-stream-json")

	// A non-JSON line (an agent warning printed outside the stream) survives.
	evs := d.Decode([]byte("Warning: no stdin data received in 3s"))
	if len(evs) != 1 || evs[0].Kind != KindRaw || evs[0].Text != "Warning: no stdin data received in 3s" {
		t.Fatalf("malformed line: got %+v, want one verbatim KindRaw", evs)
	}

	// A well-formed event type we deliberately ignore emits nothing.
	if evs := d.Decode([]byte(`{"type":"rate_limit_event","rate_limit_info":{}}`)); len(evs) != 0 {
		t.Fatalf("ignored type: got %+v, want no events", evs)
	}

	// An empty thinking block (every Claude 5 model) must still signal thinking.
	evs = d.Decode([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"xx"}]}}`))
	if len(evs) != 1 || evs[0].Kind != KindThinking {
		t.Fatalf("empty thinking: got %+v, want one KindThinking", evs)
	}

	// tool_result with is_error and no exit code.
	evs = d.Decode([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"denied","is_error":true}]}}`))
	if len(evs) != 1 || evs[0].Kind != KindToolEnd || !evs[0].IsError || evs[0].ExitCode != nil {
		t.Fatalf("error tool_result: got %+v, want KindToolEnd with IsError and no ExitCode", evs)
	}
}
