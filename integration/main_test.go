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
// The server's identity goes too: a task carries the LIVE server's public key
// and CID, and a client that inherits them rejects each test server's signed
// answer ("signature does not verify"). No build tag on this file, because the
// untagged tests in this package (the UDP forward e2e) need the same scrub.
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
		"HARNESS_SERVER_PUBKEY",
		"HARNESS_SERVER_PUBKEY_FILE",
		"HARNESS_SERVER_CID",
	} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
