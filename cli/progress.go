package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// ProgressSet writes the caller's OWN progress line (clear=true clears it).
// The request names no task: the server writes the connection's principal,
// so this only works from inside a task — an operator connection gets
// no_principal.
func (c *Client) ProgressSet(ctx context.Context, text string, clear bool) (*protocol.ProgressSetResponse, error) {
	var ps protocol.ProgressSetRequest
	ps.SetClear(clear)
	ps.SetText([]byte(text))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ProgressSet}
	req.SetProgressSet(ps)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	out := resp.ProgressSet()
	if resp.Kind != protocol.TaskControlKind_ProgressSet || out == nil {
		return nil, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	return out, nil
}

// ProgressSet (package-level) dials an agent connection for one call — the
// short-lived harness-cli inside a task.
func ProgressSet(ctx context.Context, peerCID objproto.ConnectionID, text string, clear bool) (*protocol.ProgressSetResponse, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ProgressSet(ctx, text, clear)
}

// ProgressSetStatusString is the status in the schema's snake_case.
func ProgressSetStatusString(s protocol.ProgressSetStatus) string {
	switch s {
	case protocol.ProgressSetStatus_Ok:
		return "ok"
	case protocol.ProgressSetStatus_NoPrincipal:
		return "no_principal"
	case protocol.ProgressSetStatus_BadText:
		return "bad_text"
	case protocol.ProgressSetStatus_NotFound:
		return "not_found"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}
