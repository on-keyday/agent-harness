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
	"sort"
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
	// KindError is a failure or diagnostic the agent itself reported — not a
	// runner-side classification, but the agent's own error/turn-failure
	// event (codex's "error"/"turn.failed"/item-level "error") or a failed
	// "result" envelope (claude's is_error/non-"success" subtype). Text
	// carries the message. Warning distinguishes severity: false (the
	// default) means the run ended because of this — mutually exclusive
	// with KindFinish for a given run, one outcome not both — true means a
	// non-terminal diagnostic the agent logged while continuing (e.g.
	// codex's "falling back to default model metadata" notice).
	KindError
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
	Kind Kind
	Text string // KindRaw, KindText, KindThinking, KindSessionStart (id), KindError (message)
	Tool string // KindToolStart, KindToolEnd
	Args string // KindToolStart: the tool's input, rendered for a log line —
	//                compact JSON where the agent reports structured input
	//                (claude's tool_use), a plain command string where it
	//                reports one (codex's command_execution)
	Result   string // KindToolEnd
	ExitCode *int   // KindToolEnd, when the agent reports a process exit code
	IsError  bool   // KindToolEnd, when the agent reports failure without a code
	Stats    Stats  // KindFinish
	Warning  bool   // KindError: true for a non-terminal diagnostic, false for a run-ending failure
}

// Decoder converts one line of agent stdout into zero or more events. Content it
// cannot interpret yields exactly one KindRaw event holding the line verbatim.
// Decode never returns an error: a malformed line must not fail the task.
//
// Blank or whitespace-only lines are handled per-format: the claudeStreamJSON
// and codexJSONL decoders drop them as artifacts of their respective wire
// protocols, while the passthrough decoder preserves all output byte-for-byte
// (returning exactly one KindRaw event per line) when used for non-JSON agent
// output.
type Decoder interface {
	Decode(line []byte) []Event
}

// decodersByFormat is the single source of truth for the non-empty format
// names this package recognises: every key here is a valid
// AgentProfile.LogFormat value besides "" (passthrough). NewDecoder and
// KnownFormats both read this map instead of each carrying their own copy
// of the name list, so adding a format only requires one edit and the two
// can never drift apart.
var decodersByFormat = map[string]Decoder{
	"claude-stream-json": claudeStreamJSON{},
	"codex-jsonl":        codexJSONL{},
}

// NewDecoder resolves a profile's declared log format. An empty or unrecognised
// name yields the passthrough decoder, so a misconfigured profile degrades to
// the pre-existing behaviour (raw lines) instead of failing the task.
func NewDecoder(format string) Decoder {
	if d, ok := decodersByFormat[format]; ok {
		return d
	}
	return passthrough{}
}

// HasDecoder reports whether format names a real structured decoder — i.e.
// NewDecoder(format) would return something other than the passthrough
// fallback. Callers that need stdout forwarded byte-for-byte when no
// structured decoding applies (empty or unrecognised format) should check
// this rather than routing through NewDecoder/Decode/Render: the passthrough
// decoder trims line terminators for display and Render's caller re-adds
// exactly one "\n", so a decode+render round-trip cannot reproduce the
// original bytes for a CRLF line or a final line with no terminator at all.
func HasDecoder(format string) bool {
	_, ok := decodersByFormat[format]
	return ok
}

// KnownFormats returns the non-empty AgentProfile.LogFormat values NewDecoder
// resolves to a real decoder, sorted for stable output. Configuration
// validation (runner.ProfileSet.UnrecognisedLogFormats) reports a profile's
// LogFormat as unrecognised when it is non-empty and absent from this list,
// so that check can never diverge from what NewDecoder actually accepts.
func KnownFormats() []string {
	out := make([]string, 0, len(decodersByFormat))
	for name := range decodersByFormat {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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
	case KindError:
		if e.Warning {
			// A degraded-operation notice the agent logged while continuing
			// the run — "✗" would read as "the task failed", which is not
			// what happened here.
			return "⚠ " + e.Text
		}
		return "✗ " + e.Text
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
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`

	// result
	DurationMS   int64    `json:"duration_ms"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	IsError      bool     `json:"is_error"`
	Errors       []string `json:"errors"` // set on SDKResultError; absent (nil) on SDKResultSuccess
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
		// SDKResultSuccess (subtype "success") is the only shape that means
		// the run actually succeeded. Every other shape — is_error true, or
		// any other subtype (error_during_execution, error_max_turns,
		// error_max_budget_usd, error_max_structured_output_retries) — is a
		// failed run and must not also render the "✓ ..." finish line: a run
		// reports one outcome, not two.
		if env.IsError || env.Subtype != "success" {
			text := env.Subtype
			if len(env.Errors) > 0 {
				text += ": " + strings.Join(env.Errors, "; ")
			}
			return []Event{{Kind: KindError, Text: text}}
		}
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

// codexEnvelope is the subset of codex exec --json this decoder reads.
type codexEnvelope struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"` // top-level "error" event's text
	Item     struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Message          string `json:"message"` // item-level "error" item's text
	} `json:"item"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"` // "turn.failed" event's error
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
			// Tool is deliberately not set: Render's KindToolEnd branch does
			// not print it, and the preceding KindToolStart line already
			// named the tool.
			return []Event{{
				Kind:     KindToolEnd,
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
		case "error":
			// An item-level diagnostic, distinct from turn.failed: verified
			// live against a bogus model name (captured in
			// testdata/codex-jsonl-error.jsonl) that codex emits this
			// mid-turn while it keeps going — e.g. "Model metadata ... not
			// found. Defaulting to fallback metadata; this can degrade
			// performance and cause issues." The run has not ended, so it
			// renders as a warning, not the "✗" used for a genuine
			// run-ending failure.
			return []Event{{Kind: KindError, Text: env.Item.Message, Warning: true}}
		default:
			return nil
		}
	case "turn.completed":
		return []Event{{Kind: KindFinish, Stats: Stats{
			InputTokens:  env.Usage.InputTokens,
			OutputTokens: env.Usage.OutputTokens,
		}}}
	case "error":
		// A top-level protocol/transport-level error, distinct from a
		// turn-scoped failure. Verified live against an unauthenticated
		// endpoint: codex emitted a transient reconnect notice this way
		// while retrying a dropped stream, so this is not necessarily
		// terminal — it is still reported, not suppressed, and per Decoder's
		// contract it never fails the task on its own.
		return []Event{{Kind: KindError, Text: env.Message}}
	case "turn.failed":
		// Nesting (error.message, not a top-level message like the "error"
		// case above) verified live against a real capture — a bogus model
		// name reliably induces it — not just inferred from documentation;
		// see testdata/codex-jsonl-error.jsonl. Unlike the item-level and
		// top-level error cases, a failed turn genuinely ends the run.
		return []Event{{Kind: KindError, Text: env.Error.Message}}
	default:
		return nil
	}
}
