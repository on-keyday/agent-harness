package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Submit asks the server to enqueue a new task. Returns the assigned task ID
// (32 hex chars). Method form: callable repeatedly without re-dialing — used
// by long-lived consumers (tui, wasm) that hold one *Client for the lifetime
// of the process.
func (c *Client) Submit(ctx context.Context, repo, prompt string) (string, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit}
	sub := protocol.SubmitRequest{}
	sub.SetRepoPath([]byte(repo))
	sub.SetPrompt([]byte(prompt))
	req.SetSubmit(sub)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return "", err
	}
	s := resp.Submit()
	if resp.Kind != protocol.TaskControlKind_Submit || s == nil {
		return "", fmt.Errorf("unexpected response: %+v", resp)
	}
	return hex.EncodeToString(s.TaskId.Id[:]), nil
}

// Submit (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli) where the per-call dial
// cost is acceptable. Long-lived consumers should hold a *Client and call
// (*Client).Submit instead.
func Submit(ctx context.Context, peerCID objproto.ConnectionID, repo, prompt string) (string, error) {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Submit(ctx, repo, prompt)
}
