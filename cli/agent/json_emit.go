package agent

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/agentboard"
)

// emitMessageLine writes one JSON-Lines record describing a delivered
// message. payload_b64 echoes the exact bytes so the consumer can recover
// them, and the body is additionally rendered readably: embedded raw under
// "payload" when the bytes parse as JSON, or decoded into "payload_text" when
// they are merely valid UTF-8.
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
func emitMessageLine(w io.Writer, m agentboard.DeliveredMessage, payload []byte) {
	emitMessageRecord(w, m, payload, false)
}

// hookInlineLimit is the largest payload the hook modes splice into the
// agent's prompt. Its value is the board's historical per-message limit, so
// every message that arrives inline today still does: the guard bounds only
// what a raised --agentboard-max-payload newly admits.
const hookInlineLimit = 64 * 1024

// emitMessageLineForHook is emitMessageLine for --stop-hook and
// --user-prompt-submit-hook. Those modes are the only consumer that cannot
// decline a payload — their output is spliced into the agent's next prompt, so
// an inlined body is spent context whether the agent wanted it or not. Past
// the limit the record describes the message and says how to fetch it instead.
func emitMessageLineForHook(w io.Writer, m agentboard.DeliveredMessage, payload []byte) {
	emitMessageRecord(w, m, payload, true)
}

// emitMessageRecord writes the JSON-Lines record. forHook marks the two modes
// whose output is spliced into the agent's next prompt, and it changes the
// body twice over: an over-limit body is replaced by its size and a command
// that re-reads it, and payload_b64 is dropped whenever a readable rendering
// was emitted alongside it.
//
// The readable rendering is the point, not an ergonomic extra. Base64 is a
// body no reader can read: a model handed nothing else does not shell out to
// decode it, it "reads" the blob and confabulates — a wrong instruction rather
// than a missing one. A JSON payload always had "payload"; a prose one had
// nothing until payload_text, and prose is what a relayed human instruction
// is. Dropping payload_b64 under forHook then also recovers the inflation it
// costs (4/3, or 7/3 when a JSON body is embedded raw as well); the exact
// bytes stay reachable through the plain read and `agent read <seq>`, neither
// of which is spliced into anyone's context.
//
// It takes the whole DeliveredMessage rather than its fields one at a time:
// every caller was unpacking the same nine, several of them adjacent strings,
// and reply_to_topic would have made a tenth that a misordered call site could
// not fail to compile on.
func emitMessageRecord(w io.Writer, m agentboard.DeliveredMessage, payload []byte, forHook bool) {
	seq := m.Seq
	rec := map[string]any{
		"seq":         seq,
		"in_reply_to": m.InReplyTo,
		"topic":       string(m.Topic),
		"from": map[string]any{
			"runner_id": boardRunnerIDString(m.FromRunnerId),
			"task_id":   hex.EncodeToString(m.FromTaskId.Id[:]),
			"hostname":  string(m.FromHostname),
			"agent":     string(m.FromAgentProfile),
		},
	}
	// Omitted when empty: absent means "the sender declared nothing, so a
	// reply comes back to it" — the overwhelmingly common case, and one every
	// reader already assumes. Emitting "" on every record would spend a field
	// on saying nothing happened.
	if len(m.ReplyToTopic) > 0 {
		rec["reply_to_topic"] = string(m.ReplyToTopic)
	}
	if forHook && len(payload) > hookInlineLimit {
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
	readable := false
	switch {
	case len(payload) == 0:
		// Nothing to render: payload_b64 "" already says the same nothing, and
		// keeping it there leaves one field a consumer can always address.
	case json.Valid(payload):
		rec["payload"] = json.RawMessage(payload)
		readable = true
	case utf8.Valid(payload):
		// json.Marshal escapes every byte below 0x20, so a body carrying
		// newlines or ANSI sequences can neither break the one-record-per-line
		// framing nor reach a terminal raw.
		rec["payload_text"] = string(payload)
		readable = true
	}
	if !forHook || !readable {
		rec["payload_b64"] = base64.StdEncoding.EncodeToString(payload)
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
