package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Submit asks the server to enqueue a new task. Returns the assigned task ID
// (32 hex chars). Method form: callable repeatedly without re-dialing — used
// by long-lived consumers (tui, wasm) that hold one *Client for the lifetime
// of the process.
func (c *Client) Submit(ctx context.Context, repo, prompt string) (string, error) {
	return c.SubmitWithSelectorAndArgs(ctx, repo, prompt, protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, "")
}

// SubmitWithSelector is the same as Submit but accepts an explicit runner
// selector. Callers that want the Any-runner behaviour can use Submit directly.
// extraArgs default to none; use SubmitWithSelectorAndArgs to forward a
// per-task list of CLI args to the spawned claude process.
func (c *Client) SubmitWithSelector(ctx context.Context, repo, prompt string, sel protocol.RunnerSelector) (string, error) {
	return c.SubmitWithSelectorAndArgs(ctx, repo, prompt, sel, nil, "")
}

// SubmitWithSelectorAndArgs is the full-featured form: selector pinning plus
// per-task extraArgs forwarded verbatim to the runner, plus an optional
// resumeTaskID (hex; "" means new task) that re-uses an existing terminal
// task's id and worktree branch so claude's project key matches the previous
// run. extraArgs default to none.
func (c *Client) SubmitWithSelectorAndArgs(ctx context.Context, repo, prompt string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string) (string, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit}
	sub := protocol.SubmitRequest{}
	sub.SetRepoPath([]byte(repo))
	sub.SetPrompt([]byte(prompt))
	sub.Selector = sel
	sub.ExtraArgs = protocol.ClaudeArgsFromStrings(extraArgs)
	if resumeTaskID != "" {
		tid, err := parseTaskIDHex(resumeTaskID)
		if err != nil {
			return "", fmt.Errorf("submit: parse resume id: %w", err)
		}
		sub.ResumeTaskId = tid
	}
	req.SetSubmit(sub)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return "", err
	}
	s := resp.Submit()
	if resp.Kind != protocol.TaskControlKind_Submit || s == nil {
		return "", fmt.Errorf("unexpected response: %+v", resp)
	}
	if err := submitStatusError(s); err != nil {
		return "", err
	}
	return hex.EncodeToString(s.TaskId.Id[:]), nil
}

// submitStatusError converts a non-Ok SubmitResponse.Status into a Go error.
// The ErrorMsg field (populated by the server for AmbiguousRunner) is included
// when present.
func submitStatusError(s *protocol.SubmitResponse) error {
	switch s.Status {
	case protocol.SubmitStatus_Ok:
		return nil
	case protocol.SubmitStatus_NoRunner:
		return fmt.Errorf("submit no_runner: no runner is available for this repo")
	case protocol.SubmitStatus_AmbiguousRunner:
		msg := strings.TrimSpace(string(s.ErrorMsg))
		if msg != "" {
			return fmt.Errorf("submit ambiguous_runner: %s", msg)
		}
		return fmt.Errorf("submit ambiguous_runner: multiple runners match; pin one with --runner/--host/--ip")
	case protocol.SubmitStatus_PinnedNotFound:
		return fmt.Errorf("submit pinned_not_found: the specified runner was not found")
	case protocol.SubmitStatus_ResumeNotFound:
		return fmt.Errorf("submit resume_not_found: the specified resume task id is unknown (was it pruned, or is the kind a mismatch?)")
	case protocol.SubmitStatus_ResumeNotTerminal:
		return fmt.Errorf("submit resume_not_terminal: the resume target is still queued/running (or another resume is already in flight)")
	case protocol.SubmitStatus_InternalError:
		msg := strings.TrimSpace(string(s.ErrorMsg))
		if msg != "" {
			return fmt.Errorf("submit internal_error: %s", msg)
		}
		return fmt.Errorf("submit internal_error")
	default:
		return fmt.Errorf("submit error (status=%d): %s", s.Status, string(s.ErrorMsg))
	}
}

// parseTaskIDHex decodes a 32-char lowercase hex string into a protocol.TaskID.
// Used by Submit and OpenInteractive callers to convert user-supplied --resume
// arguments into the wire form. Accepts upper/mixed case (hex.DecodeString
// is case-insensitive). Returns an error for any length other than 32 hex
// digits (16 raw bytes) so the server doesn't see a malformed all-zero id by
// accident.
func parseTaskIDHex(s string) (protocol.TaskID, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return protocol.TaskID{}, err
	}
	if len(raw) != 16 {
		return protocol.TaskID{}, fmt.Errorf("expected 32 hex chars (16 bytes), got %d bytes", len(raw))
	}
	var t protocol.TaskID
	copy(t.Id[:], raw)
	return t, nil
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
