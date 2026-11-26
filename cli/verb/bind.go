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
// It runs only when --bind is given, and a given --bind with no id in it is
// refused rather than read as unbound: a script's unset variable must not
// enroll the widest key. An unbound key is enroll without --bind.
func ParseBindFlag(s string) ([]protocol.TaskID, error) {
	ids, err := ParseTaskIDList(s)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no task id given; omit --bind for an unbound key")
	}
	if len(ids) > maxBindTasks {
		return nil, fmt.Errorf("at most %d tasks, got %d", maxBindTasks, len(ids))
	}
	return ids, nil
}
