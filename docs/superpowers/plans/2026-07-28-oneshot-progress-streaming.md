# Oneshot Agent Progress Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a oneshot agent task emit readable progress lines to its log topic while it runs, instead of nothing until it exits — and stop the never-EOF stdin pipe that hangs codex oneshot tasks.

**Architecture:** The runner asks each agent CLI for structured events (`claude --output-format stream-json --verbose`, `codex exec --json`), decodes them in a new `runner/agentlog` package into one agent-neutral event type, and renders that to text lines published on the existing `task.<id>.log` topic. No operator surface changes: CLI, TUI, and WebUI already display that topic. Separately, the oneshot stdin pipe is deleted (it exists only for a wake mechanism that does not work), and the TUI's history fetch gains a generation guard so a re-follow cannot fold a second copy of the log into the pane.

**Tech Stack:** Go 1.x (stdlib `encoding/json` only), Python 3 stdlib for `scripts/agent_presets.py`, bubbletea for the TUI.

**Spec:** `docs/superpowers/specs/2026-07-28-oneshot-progress-streaming-design.md`

## Global Constraints

- Work in the harness worktree this plan lives in. Do NOT write via absolute paths under `/home/kforfk/workspace/remote-agent-harness/<rel>` — those resolve to the parent repo's main checkout and the work will silently land on the wrong branch.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.
- This repository is public. No LAN addresses, hostnames, ports, usernames, or absolute local paths in any committed file.
- Build hygiene: compile-check with `go build ./...` or `go vet ./<pkg>`. NEVER bare `go build ./cmd/<x>/` — it drops an executable in the worktree root. The worktree must be exactly as clean after your checks as before.
- Module path is `github.com/on-keyday/agent-harness`.
- Verify with make targets (`make check`, `make wasm-check`), not ad-hoc `go build ./...` alone.
- Decoders must never fail a task. A line that cannot be interpreted becomes exactly one `KindRaw` event holding the line verbatim.
- Recognised log-format names, complete and final: `""` (passthrough), `"claude-stream-json"`, `"codex-jsonl"`. Any other value resolves to passthrough.

---

### Task 1: `runner/agentlog` event model, passthrough decoder, renderer

**Files:**
- Create: `runner/agentlog/agentlog.go`
- Create: `runner/agentlog/agentlog_test.go`
- Already present (committed with this plan, do not regenerate): `runner/agentlog/testdata/claude-stream-json.jsonl`, `runner/agentlog/testdata/codex-jsonl.jsonl`

**Interfaces:**
- Consumes: nothing.
- Produces: `agentlog.Kind` constants (`KindRaw`, `KindSessionStart`, `KindThinking`, `KindToolStart`, `KindToolEnd`, `KindText`, `KindFinish`), `agentlog.Stats`, `agentlog.Event`, `agentlog.Decoder` interface with `Decode(line []byte) []Event`, `agentlog.NewDecoder(format string) Decoder`, `agentlog.Render(e Event) string`. Tasks 2, 3, and 5 all build on these exact names.

- [ ] **Step 1: Write the failing test**

Create `runner/agentlog/agentlog_test.go`:

```go
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
		if r == '\uFFFD' {
			t.Fatal("truncation split a multi-byte rune")
		}
	}
}

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/agentlog/ -run 'TestPassthrough|TestUnknownFormat|TestRender' -v`
Expected: FAIL — the package does not compile (`undefined: NewDecoder`, `undefined: Event`, …).

- [ ] **Step 3: Write minimal implementation**

Create `runner/agentlog/agentlog.go`:

```go
// Package agentlog decodes the structured event streams that agent CLIs emit
// on stdout (claude's --output-format stream-json, codex's --json) into one
// agent-neutral event type, and renders that type as human-readable log lines.
//
// The runner publishes those lines on a task's log topic, so every operator
// surface (CLI, TUI, WebUI) shows progress without knowing anything about a
// particular agent's JSON schema. Decoding here rather than in the UIs is
// deliberate: it keeps the four display surfaces unchanged.
package agentlog

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind discriminates the agent-neutral events a Decoder can emit.
type Kind int

const (
	// KindRaw is a line the decoder could not interpret. Text holds it verbatim.
	KindRaw Kind = iota
	KindSessionStart
	KindThinking
	KindToolStart
	KindToolEnd
	KindText
	KindFinish
)

// Stats carries whatever the agent reported when its run finished. Fields the
// agent does not report stay zero: claude reports duration and cost, codex
// reports token counts. Render prints only the non-zero ones.
type Stats struct {
	DurationMS   int64
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
}

// Event is one agent-neutral log event.
//
// ExitCode and IsError are separate because the agents report tool failure
// differently and neither maps onto the other without inventing information:
// codex's command_execution carries a real process exit code, claude's
// tool_result carries only a boolean for tools that never ran a process.
type Event struct {
	Kind     Kind
	Text     string // KindRaw, KindText, KindThinking, KindSessionStart (id)
	Tool     string // KindToolStart, KindToolEnd
	Args     string // KindToolStart: compact JSON of the tool input
	Result   string // KindToolEnd
	ExitCode *int   // KindToolEnd, when the agent reports a process exit code
	IsError  bool   // KindToolEnd, when the agent reports failure without a code
	Stats    Stats  // KindFinish
}

// Decoder converts one line of agent stdout into zero or more events. A line it
// cannot interpret yields exactly one KindRaw event holding the line verbatim.
// Decode never returns an error: a malformed line must not fail the task.
type Decoder interface {
	Decode(line []byte) []Event
}

// NewDecoder resolves a profile's declared log format. An empty or unrecognised
// name yields the passthrough decoder, so a misconfigured profile degrades to
// the pre-existing behaviour (raw lines) instead of failing the task.
func NewDecoder(format string) Decoder {
	switch format {
	case "claude-stream-json":
		return claudeStreamJSON{}
	case "codex-jsonl":
		return codexJSONL{}
	default:
		return passthrough{}
	}
}

type passthrough struct{}

func (passthrough) Decode(line []byte) []Event {
	return []Event{{Kind: KindRaw, Text: strings.TrimRight(string(line), "\r\n")}}
}

// raw is the shared fallback for a line a format-specific decoder cannot parse.
func raw(line []byte) []Event {
	return passthrough{}.Decode(line)
}

// maxFieldBytes caps how much of a tool's arguments or result reaches the log.
// Tool payloads are unbounded (a file read, a large diff); the log is a
// progress feed, not a transcript.
const maxFieldBytes = 200

// truncate shortens s to at most maxFieldBytes bytes without splitting a rune,
// appending an ellipsis when it cut anything.
func truncate(s string) string {
	if len(s) <= maxFieldBytes {
		return s
	}
	cut := maxFieldBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Render formats one event as a single log line, without a trailing newline.
// The format is identical for every agent.
func Render(e Event) string {
	switch e.Kind {
	case KindSessionStart:
		return "▶ session " + e.Text
	case KindThinking:
		// Deliberately fixed: the Claude 5 family returns no thinking text at
		// all (thinking.display defaults to "omitted" and the claude CLI has no
		// flag to change it), so this is a liveness signal, not a content line.
		return "· thinking"
	case KindToolStart:
		return "→ " + e.Tool + ": " + truncate(e.Args)
	case KindToolEnd:
		out := "← " + truncate(e.Result)
		if e.ExitCode != nil && *e.ExitCode != 0 {
			out += fmt.Sprintf(" (exit %d)", *e.ExitCode)
		} else if e.IsError {
			out += " [error]"
		}
		return out
	case KindFinish:
		var parts []string
		if e.Stats.DurationMS != 0 {
			parts = append(parts, fmt.Sprintf("%dms", e.Stats.DurationMS))
		}
		if e.Stats.CostUSD != 0 {
			parts = append(parts, fmt.Sprintf("$%.6f", e.Stats.CostUSD))
		}
		if e.Stats.InputTokens != 0 || e.Stats.OutputTokens != 0 {
			parts = append(parts, fmt.Sprintf("%d in / %d out", e.Stats.InputTokens, e.Stats.OutputTokens))
		}
		if len(parts) == 0 {
			return "✓ done"
		}
		return "✓ " + strings.Join(parts, " ")
	default: // KindRaw, KindText
		return e.Text
	}
}
```

The `claudeStreamJSON` and `codexJSONL` types do not exist yet — add temporary stubs at the bottom of the same file so the package compiles; Tasks 2 and 3 replace their bodies:

```go
type claudeStreamJSON struct{}

func (claudeStreamJSON) Decode(line []byte) []Event { return raw(line) }

type codexJSONL struct{}

func (codexJSONL) Decode(line []byte) []Event { return raw(line) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/agentlog/ -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Commit**

```bash
git add runner/agentlog/agentlog.go runner/agentlog/agentlog_test.go
git commit -m "feat(agentlog): agent-neutral log event model and renderer"
```

---

### Task 2: claude `stream-json` decoder

**Files:**
- Modify: `runner/agentlog/agentlog.go` (replace the `claudeStreamJSON` stub)
- Create: `runner/agentlog/claude_test.go`
- Read-only fixture: `runner/agentlog/testdata/claude-stream-json.jsonl`

**Interfaces:**
- Consumes: `Event`, `Kind` constants, `Decoder`, `raw()` from Task 1.
- Produces: nothing new — `NewDecoder("claude-stream-json")` starts returning real events.

The fixture is a real, scrubbed `claude -p --output-format stream-json --verbose` capture. Its 10 lines are, in order: `system/init`, `system/thinking_tokens` ×2 (an event type the decoder ignores), 4 × `assistant` (whose content blocks are `thinking`, `tool_use`, `thinking`, `text`), 1 × `user` (a `tool_result`), `rate_limit_event` (ignored), and `result/success`.

- [ ] **Step 1: Write the failing test**

Create `runner/agentlog/claude_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/agentlog/ -run TestClaude -v`
Expected: FAIL — the stub returns `KindRaw`, so `TestClaudeDecoderGolden` reports 10 lines instead of 6.

- [ ] **Step 3: Write minimal implementation**

Replace the `claudeStreamJSON` stub in `runner/agentlog/agentlog.go` with:

```go
// claudeEnvelope is the subset of claude's stream-json line shape this decoder
// reads. Fields it does not name are ignored, so new event types added by a
// future claude release degrade to "no events" rather than to noise.
type claudeEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	SessionID string `json:"session_id"`

	// assistant and user both carry a message with content blocks.
	Message struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking *string         `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
			IsError  bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`

	// result
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type claudeStreamJSON struct{}

func (claudeStreamJSON) Decode(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	var env claudeEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return raw(line)
	}
	switch env.Type {
	case "system":
		if env.Subtype == "init" {
			return []Event{{Kind: KindSessionStart, Text: env.SessionID}}
		}
		return nil
	case "assistant", "user":
		var out []Event
		for _, b := range env.Message.Content {
			switch b.Type {
			case "thinking":
				// Keyed on the block's presence, not its content: every Claude 5
				// model returns an empty thinking string.
				out = append(out, Event{Kind: KindThinking})
			case "text":
				if b.Text != "" {
					out = append(out, Event{Kind: KindText, Text: b.Text})
				}
			case "tool_use":
				out = append(out, Event{Kind: KindToolStart, Tool: b.Name, Args: jsonToText(b.Input)})
			case "tool_result":
				out = append(out, Event{Kind: KindToolEnd, Result: jsonToText(b.Content), IsError: b.IsError})
			}
		}
		return out
	case "result":
		return []Event{{Kind: KindFinish, Stats: Stats{
			DurationMS: env.DurationMS,
			CostUSD:    env.TotalCostUSD,
		}}}
	default:
		return nil
	}
}

// jsonToText renders a tool input or tool result for the log. A JSON string
// becomes its unquoted value (tool results are usually plain text); anything
// else is kept as compact JSON.
func jsonToText(rm json.RawMessage) string {
	if len(rm) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(rm, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, rm); err != nil {
		return string(rm)
	}
	return buf.String()
}
```

Add `"bytes"` and `"encoding/json"` to the file's import block.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/agentlog/ -v`
Expected: PASS, including the Task 1 tests.

- [ ] **Step 5: Commit**

```bash
git add runner/agentlog/agentlog.go runner/agentlog/claude_test.go runner/agentlog/testdata/claude-stream-json.jsonl
git commit -m "feat(agentlog): decode claude stream-json events"
```

---

### Task 3: codex `--json` decoder

**Files:**
- Modify: `runner/agentlog/agentlog.go` (replace the `codexJSONL` stub)
- Create: `runner/agentlog/codex_test.go`
- Read-only fixture: `runner/agentlog/testdata/codex-jsonl.jsonl`

**Interfaces:**
- Consumes: `Event`, `Kind` constants, `Decoder`, `raw()` from Task 1, and the `decodeFixture` helper defined in `claude_test.go` (same package).
- Produces: nothing new — `NewDecoder("codex-jsonl")` starts returning real events.

The fixture is a real `codex exec --json` capture: `thread.started`, `turn.started`, `item.completed`(agent_message), `item.started`(command_execution), `item.completed`(command_execution), `item.completed`(agent_message), `turn.completed`.

- [ ] **Step 1: Write the failing test**

Create `runner/agentlog/codex_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/agentlog/ -run TestCodex -v`
Expected: FAIL — the stub emits `KindRaw`, so no session/tool/finish lines are found.

- [ ] **Step 3: Write minimal implementation**

Replace the `codexJSONL` stub in `runner/agentlog/agentlog.go` with:

```go
// codexEnvelope is the subset of codex exec --json this decoder reads.
type codexEnvelope struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
	} `json:"item"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type codexJSONL struct{}

func (codexJSONL) Decode(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	var env codexEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return raw(line)
	}
	switch env.Type {
	case "thread.started":
		return []Event{{Kind: KindSessionStart, Text: env.ThreadID}}
	case "item.started":
		if env.Item.Type == "command_execution" {
			return []Event{{Kind: KindToolStart, Tool: env.Item.Type, Args: env.Item.Command}}
		}
		return nil
	case "item.completed":
		switch env.Item.Type {
		case "command_execution":
			return []Event{{
				Kind:     KindToolEnd,
				Tool:     env.Item.Type,
				Result:   strings.TrimRight(env.Item.AggregatedOutput, "\r\n"),
				ExitCode: env.Item.ExitCode,
			}}
		case "agent_message":
			if env.Item.Text == "" {
				return nil
			}
			return []Event{{Kind: KindText, Text: env.Item.Text}}
		case "reasoning":
			return []Event{{Kind: KindThinking}}
		default:
			return nil
		}
	case "turn.completed":
		return []Event{{Kind: KindFinish, Stats: Stats{
			InputTokens:  env.Usage.InputTokens,
			OutputTokens: env.Usage.OutputTokens,
		}}}
	default:
		return nil
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/agentlog/ -v`
Expected: PASS, all three test files.

- [ ] **Step 5: Commit**

```bash
git add runner/agentlog/agentlog.go runner/agentlog/codex_test.go runner/agentlog/testdata/codex-jsonl.jsonl
git commit -m "feat(agentlog): decode codex exec --json events"
```

---

### Task 4: declare the log format on an agent profile

**Files:**
- Modify: `runner/agent_profile.go`
- Modify: `runner/agent_profile_test.go`
- Modify: `cmd/agent-runner/main.go:43-48` (Config fields), `:105-112` (flag registration), `:239-245` (default profile literal)

**Interfaces:**
- Consumes: the format-name set from Task 1 (`""`, `"claude-stream-json"`, `"codex-jsonl"`).
- Produces: `AgentProfile.LogFormat string`, the `logFormat` key in `--agent-profiles` JSON, and the `--agent-log-format` flag. Task 5 reads `agentProfile.LogFormat`; Task 7 emits both.

- [ ] **Step 1: Write the failing test**

Append to `runner/agent_profile_test.go`:

```go
func TestParseAgentProfilesJSONCarriesLogFormat(t *testing.T) {
	got, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","logFormat":"codex-jsonl"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].LogFormat != "codex-jsonl" {
		t.Fatalf("got %+v, want one profile with LogFormat codex-jsonl", got)
	}
}

func TestNewProfileSetAcceptsUnknownLogFormat(t *testing.T) {
	// A misconfigured profile must not stop the runner from starting; the
	// decoder resolves an unrecognised name to passthrough.
	_, err := NewProfileSet(AgentProfile{Name: "claude", Bin: "claude", LogFormat: "nonsense"}, nil)
	if err != nil {
		t.Fatalf("NewProfileSet rejected an unknown log format: %v", err)
	}
}

func TestUnrecognisedLogFormats(t *testing.T) {
	ps, err := NewProfileSet(
		AgentProfile{Name: "claude", Bin: "claude", LogFormat: "claude-stream-json"},
		[]AgentProfile{
			{Name: "codex", Bin: "codex", LogFormat: "codex-jsonl"},
			{Name: "bash", Bin: "bash"},
			{Name: "weird", Bin: "weird", LogFormat: "nonsense"},
		},
	)
	if err != nil {
		t.Fatalf("NewProfileSet: %v", err)
	}
	got := ps.UnrecognisedLogFormats()
	if len(got) != 1 || got[0] != `weird: "nonsense"` {
		t.Fatalf("got %v, want exactly the weird profile", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run 'TestParseAgentProfilesJSONCarriesLogFormat|TestNewProfileSetAcceptsUnknownLogFormat|TestUnrecognisedLogFormats' -v`
Expected: FAIL to compile — `unknown field LogFormat`, `ps.UnrecognisedLogFormats undefined`.

- [ ] **Step 3: Write minimal implementation**

In `runner/agent_profile.go`, add the field to `AgentProfile` (after `ResumeInteractiveArgv`):

```go
	// LogFormat names the agentlog decoder for this agent's stdout:
	// "" (passthrough), "claude-stream-json", or "codex-jsonl". An
	// unrecognised value resolves to passthrough rather than failing, so a
	// typo degrades to raw lines instead of stopping the runner.
	LogFormat string
```

Add the JSON key to `agentProfileJSON`:

```go
	LogFormat             string   `json:"logFormat"`
```

Carry it in `ParseAgentProfilesJSON`'s `append`:

```go
			LogFormat:             r.LogFormat,
```

Add the reporting helper to `ProfileSet` (do NOT reject in `NewProfileSet` — the point is non-fatal degradation):

```go
// UnrecognisedLogFormats returns `<name>: "<value>"` for every profile whose
// LogFormat is neither empty nor a name agentlog.NewDecoder recognises.
// agent-runner logs these once at startup so a typo is visible rather than
// silently degrading to raw output.
func (ps ProfileSet) UnrecognisedLogFormats() []string {
	var out []string
	for _, p := range ps.profiles {
		switch p.LogFormat {
		case "", "claude-stream-json", "codex-jsonl":
		default:
			out = append(out, fmt.Sprintf("%s: %q", p.Name, p.LogFormat))
		}
	}
	return out
}
```

In `cmd/agent-runner/main.go`, add a Config field next to `AgentProfilesJSON`:

```go
	AgentLogFormat             string
```

Register the flag next to `--agent-profiles`:

```go
	fs.StringVar(&c.AgentLogFormat, "agent-log-format", c.AgentLogFormat, "stdout log decoder for the default agent profile: \"\" (raw), claude-stream-json, or codex-jsonl")
```

Set it on the default profile literal:

```go
		LogFormat:             cfg.AgentLogFormat,
```

And warn once after `NewProfileSet` succeeds:

```go
	if bad := profiles.UnrecognisedLogFormats(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "agent-runner: unrecognised --agent-log-format/logFormat (falling back to raw output) for %v; recognised: claude-stream-json, codex-jsonl\n", bad)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/ -run 'LogFormat' -v && go vet ./cmd/agent-runner`
Expected: PASS, and vet clean.

- [ ] **Step 5: Commit**

```bash
git add runner/agent_profile.go runner/agent_profile_test.go cmd/agent-runner/main.go
git commit -m "feat(runner): let an agent profile declare its stdout log format"
```

---

### Task 5: decode agent stdout in `Process.Run`

**Files:**
- Modify: `runner/process.go` (the `Process` struct and the `scan` closure at `:146-164`)
- Modify: `runner/session.go:498-514` (pass the profile's format)
- Modify: `runner/process_test.go`

**Interfaces:**
- Consumes: `agentlog.NewDecoder`, `agentlog.Render`, `AgentProfile.LogFormat`.
- Produces: `Process.LogFormat string`. Task 9 exercises this end to end.

- [ ] **Step 1: Write the failing test**

Append to `runner/process_test.go`:

```go
func TestProcessDecodesStdoutAndLeavesStderrRaw(t *testing.T) {
	// A fake agent that prints two claude stream-json lines on stdout and one
	// plain line on stderr, then exits.
	script := filepath.Join(t.TempDir(), "fake-agent.sh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}'\n" +
		"printf '%s\\n' 'boom' >&2\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"duration_ms\":12,\"total_cost_usd\":0}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             30 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
		LogFormat:           "claude-stream-json",
	}
	var mu sync.Mutex
	var lines []string
	exit, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, string(b))
	})
	if err != nil || exit != 0 {
		t.Fatalf("Run: exit=%d err=%v", exit, err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "")
	if !strings.Contains(joined, "[out]hello\n") {
		t.Errorf("stdout was not decoded; got:\n%s", joined)
	}
	if !strings.Contains(joined, "[out]✓ 12ms\n") {
		t.Errorf("result event was not rendered; got:\n%s", joined)
	}
	if strings.Contains(joined, `"type":"assistant"`) {
		t.Errorf("raw JSON leaked into the log; got:\n%s", joined)
	}
	if !strings.Contains(joined, "[err]boom\n") {
		t.Errorf("stderr must stay verbatim; got:\n%s", joined)
	}
}

func TestProcessPassthroughWhenNoLogFormat(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'plain output\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             30 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
	}
	var mu sync.Mutex
	var got string
	if _, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		got += string(b)
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "[out]plain output\n" {
		t.Fatalf("got %q, want the line unchanged", got)
	}
}
```

Ensure the test file imports `context`, `os`, `path/filepath`, `strings`, `sync`, `testing`, and `time`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestProcess -v`
Expected: FAIL to compile — `unknown field LogFormat in struct literal`.

- [ ] **Step 3: Write minimal implementation**

In `runner/process.go`, add to the `Process` struct:

```go
	// LogFormat selects the agentlog decoder applied to stdout. Empty means
	// raw passthrough. stderr is never decoded.
	LogFormat string
```

Replace the `scan` closure and its two `go scan(...)` calls with a stdout/stderr split:

```go
	var wg sync.WaitGroup
	// emit publishes one already-rendered line under the given stream prefix.
	emit := func(prefix, text string) {
		buf := make([]byte, 0, len(prefix)+len(text)+1)
		buf = append(buf, prefix...)
		buf = append(buf, text...)
		buf = append(buf, '\n')
		sink(buf)
	}
	// scanRaw forwards each line verbatim, preserving its original newline.
	// Used for stderr, where decoding would suppress crash output.
	scanRaw := func(r io.Reader, prefix []byte) {
		defer wg.Done()
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				buf := make([]byte, 0, len(prefix)+len(line))
				buf = append(buf, prefix...)
				buf = append(buf, line...)
				sink(buf)
			}
			if err != nil {
				return
			}
		}
	}
	// scanDecoded runs each stdout line through the profile's decoder and
	// publishes one log line per resulting event. A final partial line (no
	// trailing newline before EOF) is decoded too, so nothing is lost at exit.
	scanDecoded := func(r io.Reader) {
		defer wg.Done()
		dec := agentlog.NewDecoder(p.LogFormat)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				for _, ev := range dec.Decode(line) {
					emit("[out]", agentlog.Render(ev))
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go scanDecoded(stdout)
	go scanRaw(stderr, []byte("[err]"))
```

Add `"github.com/on-keyday/agent-harness/runner/agentlog"` to the imports.

In `runner/session.go`, add to the `&Process{...}` literal in `handleAssign`:

```go
			LogFormat:                 agentProfile.LogFormat,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/ -v`
Expected: PASS, including the pre-existing process tests.

- [ ] **Step 5: Commit**

```bash
git add runner/process.go runner/process_test.go runner/session.go
git commit -m "feat(runner): render decoded agent events into the task log"
```

---

### Task 6: delete the oneshot stdin wake path

**Files:**
- Modify: `runner/process.go` (remove `OnStdinWriter` and everything it required)
- Modify: `runner/session.go:507-513` (drop the closure)
- Modify: `runner/agentskills/harness-cli/SKILL.md` (correct the wake claim)
- Modify: `.claude/skills/harness-cli/SKILL.md` (sync the copy)
- Modify: `runner/process_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `Process` no longer has `OnStdinWriter`; `cmd.Stdin` stays nil for oneshot tasks. `Session.WakeStdin` and `taskEntry.wakeWrite` remain, set only by `handleOpenExec`.

Background: `claude -p` reads stdin once with a 3-second deadline and never again, so a marker written mid-turn is never consumed. Meanwhile `codex exec` blocks waiting for EOF on that same pipe, hanging every codex oneshot task until the 30-minute timeout. No test references `OnStdinWriter`, `wakeWrite`, or `WakeStdin`, so this removal is mechanical.

- [ ] **Step 1: Write the failing test**

Append to `runner/process_test.go`:

```go
func TestProcessLeavesStdinClosed(t *testing.T) {
	// A oneshot agent must see EOF on stdin immediately. codex blocks waiting
	// for it; a never-EOF pipe hung every codex task until the timeout.
	script := filepath.Join(t.TempDir(), "reads-stdin.sh")
	body := "#!/bin/sh\n" +
		"cat > /dev/null\n" + // returns only at EOF
		"printf 'saw eof\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             10 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
	}
	var mu sync.Mutex
	var got string
	exit, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		got += string(b)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (a non-zero exit here means the process was killed at the timeout)", exit)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got, "saw eof") {
		t.Fatalf("agent never saw stdin EOF; got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Temporarily confirm the old behaviour before deleting anything: add `OnStdinWriter: func(func([]byte) (int, error)) {}` to the struct literal in the test, run it, and watch it take the full 10-second timeout and fail. Then remove that line again — the test as written above is the one that ships.

Run: `go test ./runner/ -run TestProcessLeavesStdinClosed -v`
Expected with the temporary line: FAIL after ~10s (`exit = -1`). Without it (current code, `OnStdinWriter` nil): PASS — which is why Step 3's deletion must not regress it.

- [ ] **Step 3: Write minimal implementation**

In `runner/process.go`:

1. Delete the `OnStdinWriter` field and its doc comment from the `Process` struct.
2. Delete the entire stdin-pipe block: the `var stdinPipeW *io.PipeWriter` declaration, `watcherExitCode`, `watcherDone`, the `if p.OnStdinWriter != nil { ... }` setup before `cmd.Start()`, the `if stdinPipeW != nil { stdinPipeW.Close() }` and `close(watcherDone)` inside the `cmd.Start()` error branch, and the whole `if p.OnStdinWriter != nil { ... } else { close(watcherDone) }` block after `cmd.Start()`.
3. Delete `<-watcherDone` after `wg.Wait()`.
4. In the exit-code switch, delete the `isSyscallECHILD` branch — with no stdin copier there is no second `waitpid` to race:

```go
	exit := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
			// exit == -1 means killed by signal (e.g., SIGKILL after timeout)
		} else {
			exit = -1
		}
	}
	return exit, nil
```

5. Delete the `isSyscallECHILD` function entirely.
6. Update the `Run` doc comment: it no longer mentions a stdin pipe.
7. Drop now-unused imports (`io` may still be needed by the scan closures — let the compiler tell you; `os` is still used by `cmd.Env`).

In `runner/session.go`, delete the `OnStdinWriter: func(write func([]byte) (int, error)) { ... }` field from the `&Process{...}` literal in `handleAssign`.

In `runner/agentskills/harness-cli/SKILL.md`, correct the claim at the wake description (currently "writes a synthetic `<harness:agentboard-wake>` prompt to the agent's stdin"): the runner delivers that prompt only to **interactive** sessions, through the PTY. For a oneshot task there is no wake — pending messages arrive through the `UserPromptSubmit` hook at the start of a turn, and the agent can call `harness-cli agent inbox` itself at any time. Then copy the edited file over `.claude/skills/harness-cli/SKILL.md` so the go:embed source and the checked-in copy stay identical.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/ -v && go vet ./runner/...`
Expected: PASS, vet clean, and `TestProcessLeavesStdinClosed` completes in well under a second rather than timing out.

- [ ] **Step 5: Commit**

```bash
git add runner/process.go runner/process_test.go runner/session.go runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
git commit -m "fix(runner): stop attaching a never-EOF stdin pipe to oneshot tasks"
```

---

### Task 7: emit the new argv and log format from the `--agents` presets

**Files:**
- Modify: `scripts/agent_presets.py`
- Modify: `scripts/test_agent_presets.py`
- Modify: `.claude/commands/runner-up.md` (only if it restates argv strings — it must not)

**Interfaces:**
- Consumes: the `--agent-log-format` flag and the `logFormat` JSON key from Task 4.
- Produces: presets whose oneshot argv requests structured output.

Both argv shapes were verified against the installed CLIs before this plan was written (claude 2.1.220, codex-cli 0.145.0): claude accepts `--output-format stream-json --verbose` ahead of `-p`, and `--json` is an option of the `codex exec resume` subcommand. The new flags go **before** `{args}` so a per-task `--claude-args` can still override them (agent CLIs are last-wins).

- [ ] **Step 1: Write the failing test**

Append to `scripts/test_agent_presets.py`:

```python
def test_claude_default_profile_requests_stream_json():
    out = expand_agents_preset("claude", [])
    assert "--agent-log-format" in out
    assert out[out.index("--agent-log-format") + 1] == "claude-stream-json"
    oneshot = out[out.index("--agent-oneshot-argv") + 1]
    # Flags precede {args} so a per-task --claude-args can still override them.
    assert oneshot.startswith("--output-format stream-json --verbose {args}")
    assert oneshot.endswith("-p {prompt}")
    resume = out[out.index("--agent-resume-oneshot-argv") + 1]
    assert resume.startswith("--output-format stream-json --verbose {args}")
    assert "--continue" in resume


def test_codex_extra_profile_carries_log_format():
    out = expand_agents_preset("claude,codex", [])
    profiles = json.loads(out[out.index("--agent-profiles") + 1])
    codex = next(p for p in profiles if p["name"] == "codex")
    assert codex["logFormat"] == "codex-jsonl"
    assert codex["oneshotArgv"][:2] == ["exec", "--json"]
    assert codex["resumeOneshotArgv"][:2] == ["exec", "--json"]


def test_bash_preset_has_no_log_format():
    # bash is a shell sandbox, not a conversational agent: nothing to decode.
    out = expand_agents_preset("bash", [])
    assert out[out.index("--agent-log-format") + 1] == ""
    profiles_flag = "--agent-profiles" in out
    assert not profiles_flag
```

Ensure the test module imports `json`.

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 scripts/test_agent_presets.py` (or `python3 -m pytest scripts/test_agent_presets.py -v` if the file is pytest-shaped — match whatever the existing file uses)
Expected: FAIL — `--agent-log-format` is not emitted; `oneshotArgv` still starts with `{args}`.

- [ ] **Step 3: Write minimal implementation**

In `scripts/agent_presets.py`, add a `logFormat` key to each entry of `KNOWN_AGENT_PRESETS` and change the claude/codex argv:

```python
KNOWN_AGENT_PRESETS: dict[str, dict[str, str]] = {
    "claude": {
        "bin": "claude",
        # --output-format/--verbose precede {args} so a per-task --claude-args
        # can still override them (claude flags are last-wins).
        "oneshotArgv": "--output-format stream-json --verbose {args} -p {prompt}",
        "resumeOneshotArgv": "--output-format stream-json --verbose {args} --continue -p {prompt}",
        "resumeInteractiveArgv": "{args} --continue",
        "logFormat": "claude-stream-json",
    },
    "codex": {
        "bin": "codex",
        "oneshotArgv": "exec --json {args} {prompt}",
        "resumeOneshotArgv": "exec --json resume --last {args} {prompt}",
        "resumeInteractiveArgv": "resume --last {args}",
        "logFormat": "codex-jsonl",
    },
    "bash": {
        "bin": "bash",
        "oneshotArgv": "{args} -c {prompt}",
        "resumeOneshotArgv": "{args} -c {prompt}",
        "resumeInteractiveArgv": "{args}",
        "logFormat": "",
    },
}
```

`resumeInteractiveArgv` stays unchanged for all three: interactive sessions run under a PTY and must keep their human-facing rendering.

In `expand_agents_preset`, emit the default profile's format and carry the extra profiles':

```python
    out: list[str] = [
        "--agent-bin", default["bin"],
        "--agent-oneshot-argv", default["oneshotArgv"],
        "--agent-resume-oneshot-argv", default["resumeOneshotArgv"],
        "--agent-resume-interactive-argv", default["resumeInteractiveArgv"],
        "--agent-log-format", default["logFormat"],
    ]
```

and inside the extra-profile loop's dict literal:

```python
                    "logFormat": p["logFormat"],
```

Add `"--agent-log-format"` to `_CONFLICTING_FLAGS` so `--agents` still refuses to run beside an explicit form of the same setting.

Finally read `.claude/commands/runner-up.md` and confirm it references the table rather than restating argv strings. If it restates them, replace the literals with a pointer to `scripts/agent_presets.py` — the module docstring says that file is the single source of truth and the two must not diverge.

- [ ] **Step 4: Run test to verify it passes**

Run: `python3 scripts/test_agent_presets.py`
Expected: PASS.

Then confirm the expansion is what a runner would actually receive:

Run: `python3 scripts/runner.py up --agents claude,codex --dry-run`
Expected: the printed flags include `--agent-log-format claude-stream-json` and a `--agent-profiles` JSON whose codex entry has `"logFormat": "codex-jsonl"`.

- [ ] **Step 5: Commit**

```bash
git add scripts/agent_presets.py scripts/test_agent_presets.py .claude/commands/runner-up.md
git commit -m "feat(scripts): --agents presets request structured agent output"
```

---

### Task 8: stop the TUI folding duplicate log history

**Files:**
- Modify: `tui/client.go:48-53` (`LogHistoryMsg`), `:143-150` (`DoGetTaskLog`)
- Modify: `tui/app.go:384-402` (`LogHistoryMsg` handler), `:404-406` (`BindClientMsg`), `:1406-1424` (`followTask`), and the `App` struct
- Modify: `cmd/harness-tui/main.go:80-82` (remove the reconnect-time subscribe)
- Create: `tui/logs_history_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks — this is independent of the runner work.
- Produces: `App.logsGen int`, `LogHistoryMsg.Gen int`, `DoGetTaskLogGen(c *cli.Client, taskID string, gen int) tea.Cmd`.

Root cause: `LogHistoryMsg` is guarded only by `msg.TaskID != a.logs.TaskID()`, which always passes when the task did not change. `followTask` clears the pane and issues a fresh `GetTaskLog` with no way to invalidate one already in flight, so two calls for the same task fold two full copies of the file into the pane. Pressing Enter N times on a slow-to-fetch log yields N copies.

- [ ] **Step 1: Write the failing test**

Create `tui/logs_history_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

// A superseded history response must be discarded. Before the generation
// guard, a second followTask for the same task folded a second full copy of
// the log file into the pane — pressing Enter twice on a slow fetch showed
// everything twice.
func TestStaleLogHistoryIsDropped(t *testing.T) {
	a := New(Config{})
	taskID := "0123456789abcdef0123456789abcdef"
	a.logs.Reset(taskID)
	a.logsGen = 2 // followTask ran twice; only generation 2 is current

	var m tea.Model = a
	m, _ = m.Update(LogHistoryMsg{TaskID: taskID, Content: []byte("HISTORY\n"), Found: true, Gen: 1})
	m, _ = m.Update(LogHistoryMsg{TaskID: taskID, Content: []byte("HISTORY\n"), Found: true, Gen: 2})
	app := m.(*App)

	body := strings.Join(app.logs.lines, "")
	if n := strings.Count(body, "HISTORY"); n != 1 {
		t.Fatalf("history appears %d times, want exactly 1:\n%s", n, body)
	}
}

func TestCurrentLogHistoryIsApplied(t *testing.T) {
	a := New(Config{})
	taskID := "0123456789abcdef0123456789abcdef"
	a.logs.Reset(taskID)
	a.logsGen = 1

	var m tea.Model = a
	m, _ = m.Update(LogHistoryMsg{TaskID: taskID, Content: []byte("HISTORY\n"), Found: true, Gen: 1})
	app := m.(*App)

	if !strings.Contains(strings.Join(app.logs.lines, ""), "HISTORY") {
		t.Fatal("matching-generation history was dropped")
	}
}
```

Add whatever import alias the rest of the package uses for bubbletea (`tea "github.com/charmbracelet/bubbletea"`). If `New(Config{})` does not return a `*App`, or `Update` has a different receiver shape, mirror exactly what `tui/app_noclient_test.go` does — that file is the reference for constructing an App in a test.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run TestStaleLogHistoryIsDropped -v`
Expected: FAIL to compile — `unknown field Gen in struct literal of type LogHistoryMsg`, `a.logsGen undefined`.

- [ ] **Step 3: Write minimal implementation**

In `tui/client.go`, add the field and a generation-carrying command:

```go
type LogHistoryMsg struct {
	TaskID  string
	Content []byte
	Found   bool
	Err     error
	// Gen is the followTask generation this fetch was issued for. The app
	// drops a response whose Gen is not the current one, so a superseded
	// in-flight fetch cannot fold a second copy of the log into the pane.
	Gen int
}

// DoGetTaskLogGen fetches the historical log via the persistent client,
// stamping the caller's generation onto the reply.
func DoGetTaskLogGen(c *cli.Client, taskID string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		content, found, err := c.GetTaskLog(ctx, taskID)
		return LogHistoryMsg{TaskID: taskID, Content: content, Found: found, Err: err, Gen: gen}
	}
}
```

Delete `DoGetTaskLog` and update its only caller (`followTask`). If anything else calls it, convert those call sites too.

In `tui/app.go`, add the counter to the `App` struct:

```go
	// logsGen increments on every followTask. It stamps each GetTaskLog so a
	// response from a superseded fetch can be discarded (see LogHistoryMsg).
	logsGen int
```

Guard the handler:

```go
	case LogHistoryMsg:
		// The user may have switched tasks or re-followed between fetch and
		// arrival; only apply the response for the current task AND the
		// current generation.
		if msg.TaskID != a.logs.TaskID() || msg.Gen != a.logsGen {
			return a, nil
		}
```

(the rest of the case body is unchanged)

Bump and thread the generation in `followTask`:

```go
	a.logs.Reset(taskID)
	a.logsGen++
	if taskID == "" || a.client == nil || a.program == nil || a.appCtx == nil {
		return nil
	}
	gen := a.logsGen
	subCtx, cancel := context.WithCancel(a.appCtx)
	a.logsCancel = cancel
	return tea.Batch(
		DoGetTaskLogGen(a.client, taskID, gen),
		func() tea.Msg {
			go SubscribeTaskLog(subCtx, a.client, a.program, taskID)
			return nil
		},
	)
```

Make the app the sole owner of the log subscription. In `cmd/harness-tui/main.go`, delete:

```go
					if id := app.FollowingTaskID(); id != "" {
						go tui.SubscribeTaskLog(runCtx, handle.C, program, id)
					}
```

and instead re-follow when the new client is bound, in `tui/app.go`'s `BindClientMsg` case:

```go
	case BindClientMsg:
		a.client = msg.Client
		// Re-follow so the log subscription is re-established on the new
		// client and remains owned by a.logsCancel. Without this the pane
		// would go silent after a reconnect; with a second subscription
		// spawned from main.go it would receive every chunk twice.
		if id := a.logs.TaskID(); id != "" {
			return a, a.followTask(id)
		}
		return a, nil
```

Keep `App.FollowingTaskID` — it is still used by tests and callers.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tui/ -v && go vet ./tui/... ./cmd/harness-tui`
Expected: PASS and vet clean.

- [ ] **Step 5: Commit**

```bash
git add tui/client.go tui/app.go tui/logs_history_test.go cmd/harness-tui/main.go
git commit -m "fix(tui): drop superseded log-history responses"
```

---

### Task 9: end-to-end verification against a local dummy server and runner

**Files:**
- Create: `scripts/e2e-oneshot-progress.sh`
- No production code changes.

**Interfaces:**
- Consumes: everything from Tasks 1–8.
- Produces: a repeatable check that progress lines reach the log topic before the task finishes, and that a codex-shaped agent no longer hangs.

Run against binaries built from THIS worktree. The live fleet's server is a different host running a different build and would skip the runner changes under test.

- [ ] **Step 1: Write the failing test**

Create `scripts/e2e-oneshot-progress.sh`. It must, using only this worktree's `bin/`:

1. `make build`.
2. Start a server on an ephemeral port with a temporary `--data-dir`.
3. Start a runner configured with two profiles via `scripts/runner.sh up --agents ...`-equivalent flags, except that both `bin` values point at fake agent scripts the harness creates in a temp dir:
   - a claude-shaped script that prints a `system/init` line, sleeps 2s, prints an `assistant`/`tool_use` line, sleeps 2s, prints an `assistant`/`text` line and a `result` line — declared with `--agent-log-format claude-stream-json`;
   - a codex-shaped script that runs `cat > /dev/null` first (so it only proceeds on stdin EOF), then prints `thread.started`, an `item.completed` command_execution, an `item.completed` agent_message, and `turn.completed` — declared with `logFormat: codex-jsonl`.
4. Submit a task to each profile, tailing `harness-cli logs <task>` concurrently.
5. Assert, with a hard 30-second per-task timeout:
   - **claude profile:** a line matching `^\[out\]→ ` is observed at least 1 second BEFORE the task reaches a terminal status. This is the whole point of the feature; a progress line that only arrives with the final flush proves nothing.
   - **claude profile:** the final text line (`[out]done`) is present, and no line contains `"type":"assistant"` (no raw JSON leaked).
   - **codex profile:** the task reaches a terminal status well inside the timeout. Before Task 6 this hung until the runner's 30-minute default.
   - **bash-shaped profile with no `logFormat`:** output is byte-identical to what the script printed, prefixed with `[out]`.
6. Tear down both processes and the temp dirs on exit (trap), and `exit 1` with a readable message on any failed assertion.

Contains no absolute paths, hostnames, or ports outside the temp dir and the ephemeral port it selects.

- [ ] **Step 2: Run test to verify it fails**

Run the negative control — the point of this step is proving the check can go red:

```bash
git stash push runner/process.go            # remove only the decoder wiring
bash scripts/e2e-oneshot-progress.sh; echo "exit=$?"
git stash pop
```

Expected: non-zero exit, failing on the claude-profile progress assertion (raw JSON reaches the log instead of rendered lines). A check that passes without the feature is not a check.

- [ ] **Step 3: Write minimal implementation**

No production change. If Step 2 passed with the wiring removed, the assertions are too weak — tighten them (most likely the "before terminal status" timing assertion is not actually comparing timestamps) and repeat Step 2.

- [ ] **Step 4: Run test to verify it passes**

Run: `bash scripts/e2e-oneshot-progress.sh`
Expected: exit 0, printing one PASS line per assertion.

Then the full gate:

Run: `make check && make wasm-check && go vet ./... && go test ./...`
Expected: all green.

Run: `scripts/wire-skew-check.sh`
Expected: exit 0 (no `.bgn` changed in this plan, so it is a no-op — run it anyway).

- [ ] **Step 5: Commit**

```bash
git add scripts/e2e-oneshot-progress.sh
git commit -m "test: e2e check that oneshot progress reaches the log before exit"
```

---

## After the plan

Landing follows the repo's Mode A policy (local-trunk-authoritative, fast-forward push to `origin/main`, never cherry-pick): rebase this branch onto current `main`, fast-forward `main`, push, then `make build` in the main checkout.

Restart order: this plan changes no `.bgn` and nothing on the wire, so the server does not need restarting. Rebuild `bin/` and cycle the runner fleet (`scripts/build_and_restart_all.py`) — a runner change is not live until `bin/` is refreshed. The TUI and CLI changes take effect on next launch.

Interactive wake must be re-checked by hand after Task 6, since no automated test covers it: open an interactive session, deliver an agentboard message to it, and confirm the wake marker reaches the PTY and the agent takes a turn. That is the mechanism the oneshot deletion must not disturb.
