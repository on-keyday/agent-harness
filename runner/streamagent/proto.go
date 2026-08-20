// Package streamagent defines the neutral protocol an event-stream adapter
// speaks on its own stdio, and the claude adapter that implements it.
//
// The shape is the one in docs/superpowers/specs/2026-08-20-event-stream-agent-design.md
// §2: the runner spawns an adapter, hands it the resolved agent argv, and the
// adapter execs it. Everything vendor-specific — flag names, envelope shapes,
// the permission-prompt sentinel, subtypes that come and go — lives on the far
// side of this seam. The runner frames NDJSON and routes; it does not parse
// vendor JSON, does not know the sentinel, and does not decide approvals.
//
// # Where the schema lives, and why it is not `.bgn`
//
// An earlier version of this comment said these types move into `.bgn` "the
// moment any of this crosses the harness wire — reaching the server, a client,
// or the WAL". That was decided before thinking about what carries them, and
// it is wrong.
//
// This protocol rides INSIDE `exec/frame` Stdout payloads, which the wire
// schema already treats as opaque bytes — the same field a PTY's output goes
// through. And §2 makes the adapter protocol a PUBLIC contract with its own
// `protocol_version`: third-party adapters are a first-class use, and a `.bgn`
// definition would mean writing one requires brgen. So it stays JSON, defined
// here.
//
// That is only honest if the schema says so. `feedback_no_schema_invisible_bytes`
// is satisfied not by describing every field on the wire but by the wire
// naming its payload: Stdout frames on a stream task carry "the adapter
// protocol, version N", which is a documented and versioned thing rather than
// a convention someone has to infer.
//
// The line that does hold:
//
//   - the adapter protocol is JSON, and lives here — runner, adapter and
//     clients all read these types
//   - anything the HARNESS derives from it and keeps as its own state —
//     `pending=N` on a task, the kind, the status — is `.bgn`, because that is
//     harness state rather than a payload in transit
//
// So a field like the pending count is NOT added here. It is added to
// TaskInfo, and the runner sets it by reading this protocol.
package streamagent

import (
	"encoding/json"
)

// ProtocolVersion is sent in the adapter's opening `hello` and checked by the
// runner. §2 makes the adapter protocol a public contract — third-party
// adapters are a first-class use — so the day the neutral event grows a field
// is the day every hand-written adapter has to be able to fail loudly instead
// of silently misreading.
const ProtocolVersion = 1

// MsgKind discriminates one NDJSON line. A line has exactly one payload set;
// the kind says which.
type MsgKind string

const (
	// KindHello is the adapter's first line, before anything else.
	KindHello MsgKind = "hello"
	// KindEvent is agent progress: the neutral vocabulary of runner/agentlog.
	KindEvent MsgKind = "event"
	// KindRequest is a decision the agent is BLOCKED on. Adapter → runner.
	KindRequest MsgKind = "request"
	// KindResponse answers a request. Runner → adapter.
	KindResponse MsgKind = "response"
	// KindUser is a user turn to feed the agent. Runner → adapter.
	KindUser MsgKind = "user"
	// KindExit reports the agent process exiting. Adapter → runner, last line.
	KindExit MsgKind = "exit"
)

// Msg is one NDJSON line in either direction.
type Msg struct {
	V    int     `json:"v"`
	Kind MsgKind `json:"kind"`

	Hello    *Hello    `json:"hello,omitempty"`
	Event    *Event    `json:"event,omitempty"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	User     *UserTurn `json:"user,omitempty"`
	Exit     *Exit     `json:"exit,omitempty"`
}

// Hello opens the stream. Vendor and AgentVersion are descriptive; Protocol is
// the field that gates.
type Hello struct {
	Protocol     int    `json:"protocol"`
	Vendor       string `json:"vendor"`
	AgentVersion string `json:"agent_version,omitempty"`
	// Capabilities the ADAPTER offers, not the agent's own advertised set. The
	// probe found the agent's `system/init.capabilities` does not mention
	// can_use_tool even though the channel works, so a capabilities check is
	// never the way to detect a feature here — that lives in the adapter, and
	// what it tells the runner is what IT supports.
	Capabilities []string `json:"capabilities,omitempty"`
}

// Adapter capability names. Kept as constants because a typo in a string
// literal on one side of a seam is silent.
const (
	// CapApprovals means this adapter can surface and answer tool-approval
	// requests. An adapter without it never emits KindRequest.
	CapApprovals = "approvals"
	// CapUserTurns means the agent takes further user turns after the first.
	CapUserTurns = "user_turns"
	// CapInterrupt means a turn can be cancelled without killing the process.
	CapInterrupt = "interrupt"
)

// EventKind mirrors runner/agentlog.Kind as a string, because this crosses a
// process boundary where an integer enum is a versioning hazard: a value the
// far side does not know reads as some other event rather than as unknown.
type EventKind string

const (
	EventRaw          EventKind = "raw"
	EventSessionStart EventKind = "session_start"
	EventThinking     EventKind = "thinking"
	EventToolStart    EventKind = "tool_start"
	EventToolEnd      EventKind = "tool_end"
	EventText         EventKind = "text"
	EventFinish       EventKind = "finish"
	EventError        EventKind = "error"
)

// Event is runner/agentlog.Event plus the vendor-namespaced extras of §1.
type Event struct {
	Kind     EventKind `json:"kind"`
	Text     string    `json:"text,omitempty"`
	Tool     string    `json:"tool,omitempty"`
	Args     string    `json:"args,omitempty"`
	Result   string    `json:"result,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	IsError  bool      `json:"is_error,omitempty"`
	Warning  bool      `json:"warning,omitempty"`
	Stats    *Stats    `json:"stats,omitempty"`
	// Extras keys are vendor-namespaced ("claude.rate_limit.status"); a bare
	// key is invalid. Values are strings only — wanting structure in a value is
	// the signal to promote the field, not to embed JSON in it.
	Extras map[string]string `json:"extras,omitempty"`
}

// Stats is agentlog.Stats. A pointer on Event so "the agent reported nothing"
// and "the agent reported zeros" are different lines on the wire.
type Stats struct {
	DurationMS   int64   `json:"duration_ms,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
}

// Request is a decision the agent is blocked on. It blocks until answered:
// there is no timeout here, because §4 fixes the default as block-and-notify.
type Request struct {
	// ID is the adapter's correlation id, opaque to the runner. It is NOT the
	// vendor's request id — the adapter owns that mapping, so a vendor that
	// changes its correlation scheme does not reach the runner.
	ID          string `json:"id"`
	Tool        string `json:"tool"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	// Input is the tool's arguments, verbatim, so the WebUI can render a diff
	// for Write/Edit rather than a sentence about one.
	Input       json.RawMessage `json:"input,omitempty"`
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	Suggestions []Suggestion    `json:"suggestions,omitempty"`
}

// Suggestion is a pre-canned answer the agent offers. The only kind observed is
// a session-wide mode change, which is a STANDING decision rather than an
// answer to this one call — see §3 on why exec_cowrite covers it for now.
type Suggestion struct {
	Type        string `json:"type"`
	Mode        string `json:"mode,omitempty"`
	Destination string `json:"destination,omitempty"`
	// Raw preserves a suggestion whose Type this build does not know, so an
	// unrenderable suggestion does not become a silently dropped one.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// SuggestionSetMode is the one observed Type.
const SuggestionSetMode = "setMode"

// Behavior is the verdict on a Request.
type Behavior string

const (
	BehaviorAllow Behavior = "allow"
	BehaviorDeny  Behavior = "deny"
)

// Response answers a Request.
type Response struct {
	ID       string   `json:"id"`
	Behavior Behavior `json:"behavior"`
	// Message is the deny reason. Measured 2026-08-20: it reaches the AGENT
	// verbatim as a tool_result with is_error, so this is operator-authored
	// text entering a model's context, not a private audit note.
	Message string `json:"message,omitempty"`
	// UpdatedInput rewrites the tool's arguments before allowing. Nil means
	// "allow as requested"; the adapter echoes the original rather than
	// sending null, which the vendor treats as an empty input.
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	// AcceptSuggestion, when set, accepts the Suggestion at that index instead
	// of answering only this call. Separate from Behavior because "allow this
	// one" and "stop asking" are different acts, and §3 wants them
	// distinguishable even while one capability covers both.
	AcceptSuggestion *int `json:"accept_suggestion,omitempty"`
}

// UserTurn is text to feed the agent as a new turn.
type UserTurn struct {
	Text string `json:"text"`
}

// Exit reports the agent process ending. The adapter sends it last, then
// closes stdout.
type Exit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	// Err is set when the adapter itself failed rather than the agent exiting
	// normally — an unresolvable binary, a refused flag set.
	Err string `json:"err,omitempty"`
}
