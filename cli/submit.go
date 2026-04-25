package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Submit asks the server to enqueue a new task. Returns the assigned task ID (32 hex chars).
func Submit(ctx context.Context, addr, repo, prompt string) (string, error) {
	c, err := Dial(ctx, addr)
	if err != nil {
		return "", err
	}
	defer c.Close()

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
