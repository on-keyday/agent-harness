package verb

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// maxBindTasks is RunnerEnrollRequest.bound's limit (bound enrollment, C2);
// the server refuses more, and so does the command line, before sending.
const maxBindTasks = 16

// ParseBindFlag backs `runner enroll --bind` on every surface (the WebUI calls
// it over the wasm bridge with the raw flag text): full task ids, at most 16.
// Empty is nil: unbound.
func ParseBindFlag(s string) ([]protocol.TaskID, error) {
	ids, err := ParseTaskIDList(s)
	if err != nil {
		return nil, fmt.Errorf("--bind: %w", err)
	}
	if len(ids) > maxBindTasks {
		return nil, fmt.Errorf("--bind: at most %d tasks, got %d", maxBindTasks, len(ids))
	}
	return ids, nil
}
