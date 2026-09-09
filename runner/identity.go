package runner

import (
	"crypto/rand"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// NewRunnerID mints the identity of one runner PROCESS: 16 random bytes, held
// in memory and never written to disk.
//
// Not persisted, deliberately. A stored id would come back identical after a
// restart, and the server would then read a restart as a reconnect — it would
// believe this process still holds the tasks the dead one did. The id changing
// is the only signal that says otherwise, and the failure from getting it wrong
// would surface only after a crash.
//
// 128 random bits, so nothing coordinates them and collisions are not a thing
// to reason about. rand.Read from crypto/rand cannot fail on any platform Go
// supports (it panics internally instead), so an error return here would be one
// no caller could act on.
func NewRunnerID() protocol.RunnerID {
	var rid protocol.RunnerID
	if _, err := rand.Read(rid.Id[:]); err != nil {
		panic("runner: crypto/rand failed while minting a runner identity: " + err.Error())
	}
	return rid
}
