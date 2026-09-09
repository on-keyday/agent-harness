package server

import (
	"crypto/sha256"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// testRunnerID derives a stable runner identity from a label, so a test can
// keep naming runners "runner-1" / "A" now that the field is typed.
//
// A test that registers a RunnerEntry AND assigns a task to it must use this
// for both halves: the entry's ID stays its connection id, and its Identity is
// what an AssignedTo lookup resolves through. Deriving both from one label is
// what keeps them in agreement — the two used to be the same string, which is
// exactly the conflation the typed field removes.
func testRunnerID(label string) protocol.RunnerID {
	sum := sha256.Sum256([]byte(label))
	var rid protocol.RunnerID
	copy(rid.Id[:], sum[:])
	return rid
}
