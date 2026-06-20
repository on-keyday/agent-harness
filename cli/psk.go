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
		switch resp.Status {
		case protocol.PskAuthStatus_Ok:
			return nil
		case protocol.PskAuthStatus_BadPsk:
			return fmt.Errorf("psk: server rejected: %v", resp.Status)
		case protocol.PskAuthStatus_BadTicket:
			return fmt.Errorf("psk: server rejected agent ticket: %v", resp.Status)
		case protocol.PskAuthStatus_NoIdentity:
			return fmt.Errorf("psk: server rejected (no identity): %v", resp.Status)
		default:
			return fmt.Errorf("psk: server rejected: %v", resp.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

