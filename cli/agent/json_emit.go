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
func emitMessageLine(w io.Writer, seq uint64, topic string, payload []byte, fromRid agentboard.RunnerID, fromTid agentboard.TaskID, fromHost, fromAgent string) {
	rec := map[string]any{
		"seq":         seq,
		"topic":       topic,
		"payload_b64": base64.StdEncoding.EncodeToString(payload),
		"from": map[string]any{
			"runner_id": boardRunnerIDString(fromRid),
			"task_id":   hex.EncodeToString(fromTid.Id[:]),
			"hostname":  fromHost,
			"agent":     fromAgent,
		},
	}
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
