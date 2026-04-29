package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// emitMessageLine writes one JSON-Lines record describing a delivered
// message. payload is always echoed as base64 (payload_b64 field) so the
// consumer can recover the exact bytes. When payload is JSON-parseable, it
// is additionally embedded raw under "payload" for ergonomic chains.
//
// Without this split, json.Marshal of json.RawMessage(payload) silently
// fails for non-JSON payloads and the line is emitted blank — caught by
// dogfood when sending plain-text "hello" produced empty lines.
func emitMessageLine(w io.Writer, seq uint64, topic string, payload []byte) {
	rec := map[string]any{
		"seq":         seq,
		"topic":       topic,
		"payload_b64": base64.StdEncoding.EncodeToString(payload),
	}
	if len(payload) > 0 && json.Valid(payload) {
		rec["payload"] = json.RawMessage(payload)
	}
	line, _ := json.Marshal(rec)
	fmt.Fprintln(w, string(line))
}
