package cli

import (
	"fmt"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The kinds the running verb declared it sends (verb.VerbSpec.Sends). The
// command-line dispatch and each MCP tool call install them before running a
// verb's body; RoundTripTaskControl and BeginTaskControl refuse any other kind
// before it reaches the wire. A body that grows a request its verb does not
// declare therefore fails the first time anyone runs it, instead of leaving
// the MCP tool list (which is derived from the declaration) quietly wrong.
//
// Process-wide rather than per client: a body dials its own clients, often
// several, and the dispatch that knows the verb never sees them. One verb runs
// at a time in this process (the command line runs one; the MCP server runs
// its calls one after another). The TUI and WebUI install nothing and are not
// checked.
var expected struct {
	sync.Mutex
	verb  string
	kinds map[protocol.TaskControlKind]bool
}

// ExpectKinds installs verb's declared kinds and returns the function that
// removes them. A nil kinds slice installs nothing: a verb with no
// declaration is not checked.
func ExpectKinds(verb string, kinds []protocol.TaskControlKind) (restore func()) {
	if kinds == nil {
		return func() {}
	}
	set := make(map[protocol.TaskControlKind]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	expected.Lock()
	prevVerb, prevKinds := expected.verb, expected.kinds
	expected.verb, expected.kinds = verb, set
	expected.Unlock()
	return func() {
		expected.Lock()
		expected.verb, expected.kinds = prevVerb, prevKinds
		expected.Unlock()
	}
}

func checkExpectedKind(k protocol.TaskControlKind) error {
	expected.Lock()
	defer expected.Unlock()
	if expected.kinds == nil || expected.kinds[k] {
		return nil
	}
	return fmt.Errorf("%s: sent %v, which its declaration (VerbSpec.Sends in cli/verb/table.go) does not list", expected.verb, k)
}
