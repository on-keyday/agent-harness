package integration

import "github.com/on-keyday/agent-harness/runner"

// singleAgentProfile builds a minimal runner.ProfileSet with one profile
// ("default") at the given bin path and no argv templates (the runner falls
// back to its built-in defaults). It is the integration-suite equivalent of
// the old runner.Config.ClaudeBin single-binary field, now that
// runner.Config carries a full ProfileSet (see runner/agent_profile.go).
//
// No build tag: several suite files (e.g. activity_event_e2e_test.go) run
// without -tags integration, so this helper must be visible to both the
// tagged and untagged files in this package.
func singleAgentProfile(bin string) runner.ProfileSet {
	ps, err := runner.NewProfileSet(runner.AgentProfile{Name: "default", Bin: bin}, nil)
	if err != nil {
		// Never fails for a template-free single profile.
		panic(err)
	}
	return ps
}
