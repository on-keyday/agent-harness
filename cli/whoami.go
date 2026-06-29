package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// WhoAmIWith sends a whoami TaskControl request over an already-connected
// client and returns the server's decoded self-identity response. No
// capability is required: the answer is the caller's OWN principal plus the
// capability set the server enforces for THIS connection (callerCaps),
// resolved from the connection principal rather than anything the client
// sends. Long-lived embedders (TUI/WebUI) that already hold a *Client call
// this form; short-lived processes use WhoAmI.
func WhoAmIWith(ctx context.Context, c taskControlClient) (protocol.WhoAmIResponse, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Whoami}
	req.SetWhoami(protocol.WhoAmIRequest{Reserved: 0})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return protocol.WhoAmIResponse{}, err
	}
	if resp.Kind != protocol.TaskControlKind_Whoami {
		return protocol.WhoAmIResponse{}, fmt.Errorf("unexpected response kind: %v (want Whoami)", resp.Kind)
	}
	w := resp.Whoami()
	if w == nil {
		return protocol.WhoAmIResponse{}, fmt.Errorf("response missing Whoami variant")
	}
	return *w, nil
}

// WhoAmI dials the server and returns the decoded whoami response. Identity
// (agent vs operator) is auto-selected from the env by the shared Dial path —
// exactly as every other harness-cli subcommand, so an in-task agent reports
// its confined caps and an operator shell reports operator/all.
func WhoAmI(ctx context.Context, serverCID objproto.ConnectionID) (protocol.WhoAmIResponse, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return protocol.WhoAmIResponse{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return WhoAmIWith(ctx, c)
}

// isZeroTaskID reports whether a TaskID is all-zero (the operator / no-creator
// sentinel used throughout the protocol).
func isZeroTaskID(t protocol.TaskID) bool { return t.Id == ([16]byte{}) }

// WriteWhoAmI renders a WhoAmIResponse to out. Human form is a single line:
//
//	operator                                  caps=all
//	task=<full-hex>  by=<creator8>            caps=spawn,file_read
//
// An all-zero principal means an operator connection (no confined principal →
// full authority). JSON form emits the same fields with hex task ids ("" when
// zero) for scripting.
func WriteWhoAmI(out io.Writer, resp protocol.WhoAmIResponse, asJSON bool) error {
	operator := isZeroTaskID(resp.PrincipalTaskId)
	if asJSON {
		taskHex := ""
		if !operator {
			taskHex = hex.EncodeToString(resp.PrincipalTaskId.Id[:])
		}
		creatorHex := ""
		if !isZeroTaskID(resp.CreatorTaskId) {
			creatorHex = hex.EncodeToString(resp.CreatorTaskId.Id[:])
		}
		_, err := fmt.Fprintf(out,
			"{\"operator\":%t,\"principal_task_id\":%q,\"creator_task_id\":%q,\"capabilities\":%q}\n",
			operator, taskHex, creatorHex, CapsLabel(resp.Capabilities))
		return err
	}
	caps := "caps=" + CapsLabel(resp.Capabilities)
	if operator {
		_, err := fmt.Fprintf(out, "operator  %s\n", caps)
		return err
	}
	by := ""
	if !isZeroTaskID(resp.CreatorTaskId) {
		by = "  by=" + hex.EncodeToString(resp.CreatorTaskId.Id[:])[:8]
	}
	_, err := fmt.Fprintf(out, "task=%s%s  %s\n",
		hex.EncodeToString(resp.PrincipalTaskId.Id[:]), by, caps)
	return err
}
