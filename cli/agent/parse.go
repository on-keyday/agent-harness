package agent

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// parseAgentVerb parses one `agent <sub>` line from the shared declaration.
//
// These verbs are CLI-only by construction -- they run inside a task's Bash
// tool and read HARNESS_* env -- but their grammar belongs in the same table
// as everything else, because that is what makes `agent send`'s trailing
// payload visible to the invariant tests. cli/flagorder_test.go could not see
// it: resolvePayload reads the positionals one call deeper than its scan
// looks, so `agent send` never appeared on its flags-must-precede allowlist.
func parseAgentVerb(sub string, args []string) (verb.AgentAction, error) {
	sp, ok := verb.Lookup("agent", sub)
	if !ok {
		return verb.AgentAction{}, fmt.Errorf("agent: unknown sub-verb %q", sub)
	}
	sp = sp.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, args)
	if err != nil {
		return verb.AgentAction{}, fmt.Errorf("agent %s: %w", sub, err)
	}
	act, err := sp.BuildFunc()(b)
	if err != nil {
		return verb.AgentAction{}, err
	}
	a := act.(verb.AgentAction)
	// --server-cid is env-primary; the auth ticket is env-ONLY and has no flag
	// at all, so an agent cannot be told to present someone else's.
	a.ServerCID = sp.Resolve(b, "server-cid", os.Getenv, nil, nil)
	return a, nil
}
