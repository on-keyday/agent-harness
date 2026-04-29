package agent

import (
	"encoding/json"
	"fmt"
	"io"
)

// emitStopHookOutput writes a Claude Code Stop-hook continuation directive
// when reason is non-empty. reason becomes the body fed back as additional
// input on the continued turn. Empty reason means "no new inbox messages",
// in which case nothing is written and the agent stops normally.
func emitStopHookOutput(w io.Writer, reason string) {
	if reason == "" {
		return
	}
	rec := map[string]string{"decision": "block", "reason": reason}
	line, _ := json.Marshal(rec)
	fmt.Fprintln(w, string(line))
}
