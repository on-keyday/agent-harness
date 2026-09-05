//go:build !js

package main

import (
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// spawnOpts turns the shared action into the client's option bag.
//
// --caps / --scope / --scope-for are already parsed and merged by the verb's
// Build, which is where that grammar lives; the pointers carry "the operator
// said nothing" separately from "the operator said none", because both zero
// values are meaningful.
func spawnOpts(a verb.SpawnAction) cli.SessionOpts {
	var caps protocol.Capability
	if a.Caps != nil {
		caps = *a.Caps
	}
	var scope protocol.TaskScope
	if a.Scope != nil {
		scope = *a.Scope
	}
	sel, err := cli.BuildSelector(cli.SelectorOpts{Runner: a.Runner, Host: a.Host, IP: a.IP})
	if err != nil {
		die(err)
	}
	return cli.SessionOpts{
		Selector: sel, ExtraArgs: a.ExtraArgs, ResumeTaskID: a.ResumeTaskID,
		Caps: caps, Scope: scope, Overrides: a.Overrides,
		ResumeCapsOverride: a.ResumeTaskID != "" && a.CapsPresent,
		ScopePresent:       a.ResumeTaskID != "" && a.ScopePresent,
		ResumeConversation: a.ResumeConversation, AgentProfile: a.Agent,
	}
}
