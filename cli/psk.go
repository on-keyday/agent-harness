//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// GetPSK resolves the PSK in priority order:
//  1. HARNESS_PSK env (value)
//  2. HARNESS_PSK_FILE env (path → read file, trim whitespace)
//  3. nil (no PSK)
func GetPSK() []byte {
	if v := os.Getenv("HARNESS_PSK"); v != "" {
		return []byte(v)
	}
	if path := os.Getenv("HARNESS_PSK_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return []byte(v)
			}
		}
	}
	return nil
}

// GetOperatorPSK resolves the operator-only secret (HARNESS_OPERATOR_PSK /
// HARNESS_OPERATOR_PSK_FILE), mirroring GetPSK. It is deliberately a DISTINCT
// env var from HARNESS_PSK: an agent-runner reads HARNESS_PSK from its own env
// (cmd/agent-runner/main.go) and injects it into spawned agents, so if the
// operator secret lived in HARNESS_PSK a runner launched in the same shell would
// inherit it and leak it to agents — reopening the kind=Client → operator
// escalation. Keeping it in HARNESS_OPERATOR_PSK keeps it off that path.
func GetOperatorPSK() []byte {
	if v := os.Getenv("HARNESS_OPERATOR_PSK"); v != "" {
		return []byte(v)
	}
	if path := os.Getenv("HARNESS_OPERATOR_PSK_FILE"); path != "" {
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

// resolveBinderPSK picks the secret whose binder this process should present,
// consistent with the kind buildMergedClientHello will announce:
//   - in-task agent  → the connect psk (HARNESS_PSK, runner-injected).
//   - operator surface → the operator psk (HARNESS_OPERATOR_PSK) when set,
//     falling back to HARNESS_PSK for deployments that have not split them.
func resolveBinderPSK() []byte {
	if isAgentContext() {
		return GetPSK()
	}
	if op := GetOperatorPSK(); len(op) > 0 {
		return op
	}
	return GetPSK()
}

// buildMergedClientHello constructs the ClientHello to embed in a
// PskAuthRequest. When the in-task agent env (HARNESS_RUNNER_ID /
// HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is fully populated, kind is
// overridden to Agent with AgentInfo; otherwise the supplied operatorKind
// is used.
func buildMergedClientHello(operatorKind protocol.ClientKind) protocol.ClientHello {
	hello := protocol.ClientHello{Kind: operatorKind}
	if rid, err := cliopts.ResolveRunnerID(""); err == nil {
		if tid, err := cliopts.ResolveTaskID(""); err == nil {
			if ticket, err := cliopts.ResolveAuthTicket(); err == nil {
				info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
				info.SetHostname([]byte(cliopts.ResolveString("", "HARNESS_HOSTNAME")))
				hello.Kind = protocol.ClientKind_Agent
				hello.SetAgentInfo(info)
			}
		}
	}
	return hello
}

// SendMergedHandshake builds a PskAuthRequest{binder (or empty when psk==nil),
// role=client, client_hello = <buildMergedClientHello(operatorKind)>}, sends
// [0x45]+PskAuthRequest via sendFn, then blocks until a PskAuthResponse
// arrives on respCh or ctx is cancelled.
//
// The binder computation (HMAC-SHA512 over the objproto transcript) is
// unchanged from ComputePSKBinder; only the wire format moves from the old
// hand-built [0x45+binder] to the brgen-schematized PskAuthRequest.
//
// Error mapping:
//   - PskAuthStatus_BadPsk      → error (wrong PSK)
//   - PskAuthStatus_BadTicket   → error (binder ok, invalid agent ticket)
//   - PskAuthStatus_NoIdentity  → error (should not happen: we always embed a hello)
func SendMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, operatorKind protocol.ClientKind, respCh <-chan protocol.PskAuthResponse) error {
	req := protocol.PskAuthRequest{Role: protocol.AuthRole_Client}

	if len(psk) > 0 {
		binder, err := ComputePSKBinder(psk, transcript)
		if err != nil {
			return fmt.Errorf("psk: binder: %w", err)
		}
		if !req.SetBinder(binder) {
			return fmt.Errorf("psk: SetBinder failed (len=%d)", len(binder))
		}
	} else {
		req.SetBinder(nil) // binder_len = 0
	}

	hello := buildMergedClientHello(operatorKind)
	if !req.SetClientHello(hello) {
		return fmt.Errorf("psk: SetClientHello failed")
	}

	data, err := req.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		return fmt.Errorf("psk: encode: %w", err)
	}
	if err := sendFn(data); err != nil {
		return fmt.Errorf("psk: send: %w", err)
	}

	select {
	case resp := <-respCh:
		if resp.Status == protocol.PskAuthStatus_Ok {
			return nil
		}
		// Explicit server rejection (bad psk / bad ticket / no identity) — FATAL,
		// not retryable. Callers wrap this as *PSKAuthError.
		return &PskRejectedError{Status: resp.Status.String()}
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE (e.g. a server
		// restart interrupting the in-flight handshake). NOT a PskRejectedError,
		// so callers must treat it as a normal disconnect and reconnect.
		return ctx.Err()
	}
}

