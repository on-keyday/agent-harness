//go:build !js

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// clearAgentEnv neutralises the in-task agent env so the process looks like an
// operator surface. t.Setenv to "" is treated as unset by cliopts.Resolve*.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")
}

// setAgentEnv makes the process look like an in-task agent (valid runner-id +
// task-id + auth-ticket), the same signal buildMergedClientHello keys on.
func setAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_RUNNER_ID", "ws:127.0.0.1:8539-1")
	t.Setenv("HARNESS_TASK_ID", strings.Repeat("ab", 16))
	t.Setenv("HARNESS_AUTH_TICKET", strings.Repeat("cd", 16))
}

func TestResolveBinderPSK_OperatorPrefersOperatorPSK(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("HARNESS_PSK", "connect-secret")
	t.Setenv("HARNESS_OPERATOR_PSK", "operator-secret")

	got := resolveBinderPSK()
	if !bytes.Equal(got, []byte("operator-secret")) {
		t.Errorf("operator surface: got %q, want operator-secret", got)
	}
}

func TestResolveBinderPSK_OperatorFallsBackToConnectPSK(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("HARNESS_PSK", "connect-secret")
	t.Setenv("HARNESS_OPERATOR_PSK", "")

	got := resolveBinderPSK()
	if !bytes.Equal(got, []byte("connect-secret")) {
		t.Errorf("operator surface without operator psk: got %q, want connect-secret", got)
	}
}

// The escalation guard: even when HARNESS_OPERATOR_PSK is present in the
// environment, an in-task agent must NOT use it — it proves only the connect
// psk. (This is what stops a runner that inherited HARNESS_OPERATOR_PSK from
// handing agents an operator-capable binder; agents simply never read it.)
func TestResolveBinderPSK_AgentNeverUsesOperatorPSK(t *testing.T) {
	setAgentEnv(t)
	t.Setenv("HARNESS_PSK", "connect-secret")
	t.Setenv("HARNESS_OPERATOR_PSK", "operator-secret")

	got := resolveBinderPSK()
	if !bytes.Equal(got, []byte("connect-secret")) {
		t.Errorf("agent context: got %q, want connect-secret (must ignore operator psk)", got)
	}
}
