//go:build !js

package cli

import (
	"github.com/on-keyday/agent-harness/connauth"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/cli/cliopts"
)

// GetOperatorPSK resolves the operator-only secret (OperatorPSKEnv, then the
// file OperatorPSKFileEnv names). An agent inheriting the runner's environment
// wholesale would reopen the operator-kind escalation, which the runner closes
// by dropping exactly these two names before it spawns anything.
func GetOperatorPSK() []byte {
	if v := os.Getenv(connauth.OperatorPSKEnv); v != "" {
		return []byte(v)
	}
	if path := os.Getenv(connauth.OperatorPSKFileEnv); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return []byte(v)
			}
		}
	}
	return nil
}

// isAgentContext reports whether this process is an in-task agent — the same
// signal buildMergedClientHello uses to announce kind=Agent (runner-id +
// task-id + auth-ticket all resolvable from the env).
func isAgentContext() bool {
	if _, err := cliopts.ResolveRunnerID(""); err != nil {
		return false
	}
	if _, err := cliopts.ResolveTaskID(""); err != nil {
		return false
	}
	if _, err := cliopts.ResolveAuthTicket(); err != nil {
		return false
	}
	return true
}

// resolveBinderPSK picks the secret an operator surface's binder proves: the
// operator PSK. An in-task agent needs none — its binder is keyed by its
// ticket (SendMergedHandshake).
func resolveBinderPSK() []byte {
	if isAgentContext() {
		return nil
	}
	return GetOperatorPSK()
}
