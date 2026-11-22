package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The kinds the running verb declared it sends (verb.VerbSpec.Sends). The
// command-line dispatch and each MCP tool call put them on the context the
// verb's body runs under; every client the body dials from that context
// records them, and RoundTripTaskControl and BeginTaskControl refuse any other
// kind before it reaches the wire. A body that grows a request its verb does
// not declare therefore fails the first time anyone runs it, instead of
// leaving the MCP tool list (which is derived from the declaration) quietly
// wrong.
//
// On the context rather than process-wide: the MCP server runs calls of
// different verbs at once, each with its own declaration. The TUI and WebUI
// put nothing on their contexts and are not checked.
type expectedKinds struct {
	verb  string
	kinds map[protocol.TaskControlKind]bool
}

type expectedKindsKey struct{}

// WithExpectedKinds returns ctx carrying verb's declared kinds. A nil kinds
// slice returns ctx unchanged: a verb with no declaration is not checked.
func WithExpectedKinds(ctx context.Context, verb string, kinds []protocol.TaskControlKind) context.Context {
	if kinds == nil {
		return ctx
	}
	set := make(map[protocol.TaskControlKind]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	return context.WithValue(ctx, expectedKindsKey{}, &expectedKinds{verb: verb, kinds: set})
}

// expectedKindsFrom is what dialOn records on a client: nil when nothing was
// declared.
func expectedKindsFrom(ctx context.Context) *expectedKinds {
	e, _ := ctx.Value(expectedKindsKey{}).(*expectedKinds)
	return e
}

func (e *expectedKinds) check(k protocol.TaskControlKind) error {
	if e == nil || e.kinds[k] {
		return nil
	}
	return fmt.Errorf("%s: sent %v, which its declaration (VerbSpec.Sends in cli/verb/table.go) does not list", e.verb, k)
}
