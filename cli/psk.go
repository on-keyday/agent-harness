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

// GetOperatorPSK resolves the operator-only secret (OperatorPSKEnv /
// OperatorPSKFileEnv), mirroring GetPSK. It is deliberately a DISTINCT env var
// from HARNESS_PSK: an agent-runner reads HARNESS_PSK from its own env
// (cmd/agent-runner/main.go) and injects it into spawned agents, so if the
// operator secret lived in HARNESS_PSK it would reach every agent by design —
// reopening the kind=Client → operator escalation. The other route, an agent
// inheriting the runner's environment wholesale, is closed by the runner
// dropping exactly these two names before it spawns anything.
func GetOperatorPSK() []byte {
	if v := os.Getenv(OperatorPSKEnv); v != "" {
		return []byte(v)
	}
	if path := os.Getenv(OperatorPSKFileEnv); path != "" {
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

// EffectiveClientKind reports which principal a client dialed from THIS
// process announces: the operator kind it asked for, or Agent when the in-task
// env (HARNESS_RUNNER_ID / HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is fully
// populated and overrides it. The task id is returned too, zero for an
// operator.
//
// Exported so a surface can SAY which principal it is. Connecting as an agent
// with caps=none makes the runner list come back EMPTY, and nothing on screen
// explained that — the symptom reads as a broken pane or a lost connection.
// buildMergedClientHello makes the same decision through this function, so the
// header cannot disagree with the handshake.
func EffectiveClientKind(operatorKind protocol.ClientKind) (protocol.ClientKind, protocol.TaskID) {
	if rid, err := cliopts.ResolveRunnerID(""); err == nil && !rid.IsZero() {
		if tid, err := cliopts.ResolveTaskID(""); err == nil {
			if _, err := cliopts.ResolveAuthTicket(); err == nil {
				return protocol.ClientKind_Agent, tid
			}
		}
	}
	return operatorKind, protocol.TaskID{}
}

// buildMergedClientHello constructs the ClientHello to embed in a
// PskAuthRequest. When the in-task agent env is fully populated, kind is
// overridden to Agent with AgentInfo; otherwise the supplied operatorKind
// is used. The override decision itself lives in EffectiveClientKind.
func buildMergedClientHello(operatorKind protocol.ClientKind) protocol.ClientHello {
	hello := protocol.ClientHello{Kind: operatorKind}
	if kind, _ := EffectiveClientKind(operatorKind); kind != protocol.ClientKind_Agent {
		return hello
	}
	// Re-resolving rather than threading the values out of EffectiveClientKind:
	// that function answers WHICH principal, this one needs the credential
	// itself, and an agent env that changed between the two calls would fail
	// the handshake rather than announce a half-built identity.
	rid, err := cliopts.ResolveRunnerID("")
	if err != nil {
		return hello
	}
	tid, err := cliopts.ResolveTaskID("")
	if err != nil {
		return hello
	}
	ticket, err := cliopts.ResolveAuthTicket()
	if err != nil {
		return hello
	}
	info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
	info.SetHostname([]byte(cliopts.ResolveString("", "HARNESS_HOSTNAME")))
	hello.Kind = protocol.ClientKind_Agent
	hello.SetAgentInfo(info)
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
//   - PskAuthStatus_NoIdentity  → RETRYABLE error: we always embed a hello, so this
//     means the server could not DECODE it — in practice a wire/schema skew
//     (server older than us). Resolves when the server is upgraded; must not
//     be fatal (see PskRejectedError.Retryable).
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
		return NewPskRejectedError(resp.Status)
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE (e.g. a server
		// restart interrupting the in-flight handshake). NOT a PskRejectedError,
		// so callers must treat it as a normal disconnect and reconnect.
		return ctx.Err()
	}
}
