package agent

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/agentboard"
)

// emitMessageLine writes one JSON-Lines record describing a delivered
// message. payload is always echoed as base64 (payload_b64 field) so the
// consumer can recover the exact bytes. When payload is JSON-parseable, it
// is additionally embedded raw under "payload" for ergonomic chains.
//
// The from block carries server-attested sender info (RunnerID, TaskID,
// hostname, agent profile). It is always present, even for legacy messages
// where the bytes may be zero — that lets jq/grep consumers reliably address
// `.from.*`. An empty `agent` means the server could not attribute a runtime to
// the sender (e.g. a server-originated publish, which carries hostname
// "server"); it never means "runner default".
//
// in_reply_to is emitted on every record, 0 when the message is not a reply,
// for the same reason the from block is unconditional: a consumer can address
// the field without probing for it.
func emitMessageLine(w io.Writer, seq uint64, topic string, payload []byte, fromRid agentboard.RunnerID, fromTid agentboard.TaskID, fromHost, fromAgent string, inReplyTo uint64) {
	emitMessageRecord(w, seq, topic, payload, fromRid, fromTid, fromHost, fromAgent, inReplyTo, 0)
}

// hookInlineLimit is the largest payload the hook modes splice into the
// agent's prompt. Its value is the board's historical per-message limit, so
// every message that arrives inline today still does: the guard bounds only
// what a raised --agentboard-max-payload newly admits.
const hookInlineLimit = 64 * 1024

// emitMessageLineForHook is emitMessageLine for --stop-hook and
// --user-prompt-submit-hook. Those modes are the only consumer that cannot
// decline a payload — their output is spliced into the agent's next prompt, so
// an inlined body is spent context whether the agent wanted it or not, and the
// cost is worse than the byte count: payload_b64 inflates 4/3, and a
// JSON-parseable body is ALSO embedded raw, for 7/3 in total. Past the limit
// the record describes the message and says how to fetch it instead.
func emitMessageLineForHook(w io.Writer, seq uint64, topic string, payload []byte, fromRid agentboard.RunnerID, fromTid agentboard.TaskID, fromHost, fromAgent string, inReplyTo uint64) {
	emitMessageRecord(w, seq, topic, payload, fromRid, fromTid, fromHost, fromAgent, inReplyTo, hookInlineLimit)
}

// emitMessageRecord writes the JSON-Lines record. inlineLimit == 0 means the
// body is always carried; a positive value replaces an over-limit body with
// its size and a command that re-reads it.
func emitMessageRecord(w io.Writer, seq uint64, topic string, payload []byte, fromRid agentboard.RunnerID, fromTid agentboard.TaskID, fromHost, fromAgent string, inReplyTo uint64, inlineLimit int) {
	rec := map[string]any{
		"seq":         seq,
		"in_reply_to": inReplyTo,
		"topic":       topic,
		"from": map[string]any{
			"runner_id": boardRunnerIDString(fromRid),
			"task_id":   hex.EncodeToString(fromTid.Id[:]),
			"hostname":  fromHost,
			"agent":     fromAgent,
		},
	}
	if inlineLimit > 0 && len(payload) > inlineLimit {
		// `agent read` addresses this seq alone and never truncates, which is
		// what makes it a usable destination. Pointing at `inbox --since
		// <seq-1>` instead would re-deliver every later message too, and inbox
		// fetches a whole batch's payloads before emitting any — so the
		// pointer would pull exactly the bytes this record avoided.
		rec["payload_bytes"] = len(payload)
		rec["payload_omitted"] = true
		rec["read_with"] = fmt.Sprintf("harness-cli agent read %d", seq)
		line, _ := json.Marshal(rec)
		fmt.Fprintln(w, string(line))
		return
	}
	rec["payload_b64"] = base64.StdEncoding.EncodeToString(payload)
	if len(payload) > 0 && json.Valid(payload) {
		rec["payload"] = json.RawMessage(payload)
	}
	line, _ := json.Marshal(rec)
	fmt.Fprintln(w, string(line))
}

// boardRunnerIDString renders an agentboard.RunnerID as "transport:ip:port-unique"
// matching HARNESS_RUNNER_ID / cliopts format.
func boardRunnerIDString(r agentboard.RunnerID) string {
	ip := ""
	switch len(r.IpAddr) {
	case 4:
		ip = fmt.Sprintf("%d.%d.%d.%d", r.IpAddr[0], r.IpAddr[1], r.IpAddr[2], r.IpAddr[3])
	case 16:
		ip = "[" + hex.EncodeToString(r.IpAddr) + "]"
	}
	return fmt.Sprintf("%s:%s:%d-%d", string(r.Transport), ip, r.Port, r.UniqueNumber)
}
