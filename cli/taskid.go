package cli

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// parseTaskIDHex decodes a 32-char hex string into a protocol.TaskID. Used
// wherever a user-supplied task id (--resume, a verb's task argument) becomes
// the wire form. Accepts upper/mixed case (hex.DecodeString is
// case-insensitive). Returns an error for any length other than 32 hex digits
// (16 raw bytes) so the server doesn't see a malformed all-zero id by
// accident.
func parseTaskIDHex(s string) (protocol.TaskID, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return protocol.TaskID{}, err
	}
	if len(raw) != 16 {
		return protocol.TaskID{}, fmt.Errorf("expected 32 hex chars (16 bytes), got %d bytes", len(raw))
	}
	var t protocol.TaskID
	copy(t.Id[:], raw)
	return t, nil
}
