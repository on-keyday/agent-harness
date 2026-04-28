package agent

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func hexTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}
