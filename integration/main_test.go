//go:build integration

package integration

import (
	"os"
	"testing"
)

// TestMain strips the harness agent-context environment before any test runs.
//
// When this suite is executed from inside a harness-spawned task (e.g. an agent
// driving the repo), the runner pre-sets HARNESS_TASK_ID / HARNESS_AUTH_TICKET /
// etc. cli.Dial reads those and upgrades the connection to a confined Agent
// whose principal task is unknown to each test's in-process server — yielding
// Capability_None (spawn denied) and descendant-only visibility (runners look
// "not registered"). In clean CI these vars are absent and the suite passes;
// stripping them here makes the result identical regardless of where it runs.
//
// Tests that need a specific value (e.g. the PSK e2e tests) set it per-test via
// t.Setenv, which runs after this and restores afterward.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"HARNESS_RUNNER_ID",
		"HARNESS_TASK_ID",
		"HARNESS_AUTH_TICKET",
		"HARNESS_PSK",
		"HARNESS_OPERATOR_PSK",
		"HARNESS_PROXY_VIA_RUNNER",
	} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
