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
	"bytes"
	"encoding/json"
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

type codexJSONL struct{}

func (codexJSONL) Decode(line []byte) []Event { return raw(line) }
