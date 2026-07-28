package agentlog

import (
	"strings"
	"testing"
)

func TestCodexDecoderGolden(t *testing.T) {
	got := decodeFixture(t, "testdata/codex-jsonl.jsonl", NewDecoder("codex-jsonl"))
	// The fixture's first agent_message is a preamble the model wrote before
	// running the command; both agent messages render as plain text lines.
	if len(got) < 5 {
		t.Fatalf("got %d lines, want at least 5:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.HasPrefix(got[0], "▶ session ") {
		t.Errorf("line 0 = %q, want a session line", got[0])
	}
	var sawToolStart, sawToolEnd, sawDone, sawFinish bool
	for _, l := range got {
		switch {
		case strings.HasPrefix(l, "→ command_execution: /bin/bash -lc 'echo one'"):
			sawToolStart = true
		case l == "← one":
			sawToolEnd = true
		case l == "done":
			sawDone = true
		case strings.HasPrefix(l, "✓ ") && strings.Contains(l, " in / "):
			sawFinish = true
		}
	}
	if !sawToolStart || !sawToolEnd || !sawDone || !sawFinish {
		t.Fatalf("missing lines (start=%v end=%v done=%v finish=%v):\n%s",
			sawToolStart, sawToolEnd, sawDone, sawFinish, strings.Join(got, "\n"))
	}
}

func TestCodexDecoderExitCodeAndUnknown(t *testing.T) {
	d := NewDecoder("codex-jsonl")

	evs := d.Decode([]byte(`{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"boom\n","exit_code":2,"status":"completed"}}`))
	if len(evs) != 1 || evs[0].Kind != KindToolEnd || evs[0].ExitCode == nil || *evs[0].ExitCode != 2 {
		t.Fatalf("got %+v, want KindToolEnd with ExitCode 2", evs)
	}
	if evs[0].IsError {
		t.Error("IsError must stay false when a real exit code is present")
	}
	if Render(evs[0]) != "← boom (exit 2)" {
		t.Errorf("Render = %q", Render(evs[0]))
	}

	// turn.started carries nothing worth logging.
	if evs := d.Decode([]byte(`{"type":"turn.started"}`)); len(evs) != 0 {
		t.Fatalf("turn.started: got %+v, want no events", evs)
	}

	// An unknown item type is dropped, not rendered as noise.
	if evs := d.Decode([]byte(`{"type":"item.completed","item":{"type":"future_thing"}}`)); len(evs) != 0 {
		t.Fatalf("unknown item: got %+v, want no events", evs)
	}

	// codex prints non-JSON notices on stderr, but a stray one on stdout survives.
	evs = d.Decode([]byte("Reading additional input from stdin..."))
	if len(evs) != 1 || evs[0].Kind != KindRaw {
		t.Fatalf("non-JSON line: got %+v, want one KindRaw", evs)
	}
}

// TestCodexDecoderError is a unit test rather than an addition to
// testdata/codex-jsonl.jsonl: that fixture is a real, unedited capture of one
// successful run, and hand-appending a failure line would misrepresent it as
// recorded output.
func TestCodexDecoderError(t *testing.T) {
	d := NewDecoder("codex-jsonl")

	// Verified live: running `codex exec --json` against an unauthenticated
	// endpoint emitted exactly this line on stdout.
	evs := d.Decode([]byte(`{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized)"}`))
	if len(evs) != 1 || evs[0].Kind != KindError || evs[0].Text != "Reconnecting... 2/5 (unexpected status 401 Unauthorized)" {
		t.Fatalf("got %+v, want one KindError carrying the message", evs)
	}
	if Render(evs[0]) != "✗ Reconnecting... 2/5 (unexpected status 401 Unauthorized)" {
		t.Errorf("Render = %q", Render(evs[0]))
	}

	// turn.failed carries its message nested under "error", not top-level.
	evs = d.Decode([]byte(`{"type":"turn.failed","error":{"message":"model response stream ended unexpectedly"}}`))
	if len(evs) != 1 || evs[0].Kind != KindError || evs[0].Text != "model response stream ended unexpectedly" {
		t.Fatalf("got %+v, want one KindError carrying the turn's error message", evs)
	}
}
