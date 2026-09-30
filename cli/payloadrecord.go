package cli

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"
)

// PutPayloadFields writes a message body into a JSON-Lines record in every
// form it has: "payload" embedded raw when the bytes parse as JSON,
// "payload_text" when they are merely valid UTF-8, and "payload_b64" with the
// exact bytes. It is the one place a body record is shaped, so `agent inbox`
// / `read` / `wait` / `dispatch`, `board read --json` and both thread verbs
// cannot disagree about it — `payload_text` once reached the inbox record
// and neither board record, which left the agent-facing `agent thread --json`
// handing a prose body over as base64 alone.
//
// The readable rendering is the point, not an ergonomic extra. Base64 is a
// body no reader can read: a model handed nothing else does not shell out to
// decode it, it "reads" the blob and confabulates — a wrong instruction rather
// than a missing one. json.Marshal escapes every byte below 0x20, so a
// payload_text carrying newlines or ANSI sequences can neither break the
// one-record-per-line framing nor reach a terminal raw.
//
// dropB64WhenReadable omits payload_b64 once one of the readable forms was
// written, for a record spliced into a prompt where a second copy of the body
// is spent context. A body with no readable form keeps payload_b64 regardless:
// it is then the only body there is. An empty body writes payload_b64 "" and
// nothing else, so one body field is always addressable.
func PutPayloadFields(rec map[string]any, payload []byte, dropB64WhenReadable bool) {
	readable := false
	switch {
	case len(payload) == 0:
	case json.Valid(payload):
		rec["payload"] = json.RawMessage(payload)
		readable = true
	case utf8.Valid(payload):
		rec["payload_text"] = string(payload)
		readable = true
	}
	if !dropB64WhenReadable || !readable {
		rec["payload_b64"] = base64.StdEncoding.EncodeToString(payload)
	}
}
