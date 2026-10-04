package agent

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// emitMessageLine writes one JSON-Lines record describing a delivered
// message. The body fields are cli.PutPayloadFields': payload_b64 with the
// exact bytes, plus "payload" or "payload_text" when the bytes are readable.
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
func emitMessageLine(w io.Writer, m protocol.DeliveredMessage, payload []byte) {
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
func emitMessageLineForHook(w io.Writer, m protocol.DeliveredMessage, payload []byte) {
	emitMessageRecord(w, m, payload, true)
}

// emitMessageRecord writes the JSON-Lines record. forHook marks the two modes
// whose output is spliced into the agent's next prompt, and it changes the
// body twice over: an over-limit body is replaced by its size and a command
// that re-reads it, and payload_b64 is dropped whenever a readable rendering
// was emitted alongside it (cli.PutPayloadFields). That drop recovers the
// inflation a second copy costs (4/3, or 7/3 when a JSON body is embedded raw
// as well); the exact bytes stay reachable through the plain read and `agent
// read <seq>`, neither of which is spliced into anyone's context.
//
// It takes the whole DeliveredMessage rather than its fields one at a time:
// every caller was unpacking the same nine, several of them adjacent strings,
// and reply_to_topic would have made a tenth that a misordered call site could
// not fail to compile on.
func emitMessageRecord(w io.Writer, m protocol.DeliveredMessage, payload []byte, forHook bool) {
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
		// Beside "from", not inside it: who published is not part of the
		// sender's identity block, and a reader ignoring unknown keys at the
		// top level is unaffected.
		"sender_kind": cli.SenderKindName(m.SenderKind),
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
	cli.PutPayloadFields(rec, payload, forHook)
	line, _ := json.Marshal(rec)
	fmt.Fprintln(w, string(line))
}

// boardRunnerIDString renders an protocol.RunnerID as the 32-hex identity,
// which is also what HARNESS_RUNNER_ID and cliopts carry. It used to assemble
// "transport:ip:port-unique" by hand, because the identity was an address.
func boardRunnerIDString(r protocol.RunnerID) string {
	return hex.EncodeToString(r.Id[:])
}
