//go:build !js

package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	agentexec "github.com/on-keyday/agent-harness/exec"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// OpenInteractive asks the server to allocate an interactive PTY claude
// session in repoPath on an idle runner, splices the per-side streams, and
// returns a CommandExecutionStream wired to the client end. taskIDHex is
// the new task's id (so the caller can show it in tasks list / cancel it).
//
// On success, the caller owns the returned stream and is responsible for
// calling RemoteShell (or Stdin/Stdout/Stderr individually) and Close.
// The runner's exec.ExecuteCommand drives PTY lifecycle on the other side.
func (c *Client) OpenInteractive(ctx context.Context, repoPath string) (*agentexec.CommandExecutionStream, string, error) {
	return c.OpenInteractiveWithSelectorAndArgs(ctx, repoPath, protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, "")
}

// OpenInteractiveWithSelector is the same as OpenInteractive but accepts an
// explicit runner selector. extraArgs default to none; use the AndArgs form
// to forward per-task CLI args to the spawned claude process.
func (c *Client) OpenInteractiveWithSelector(ctx context.Context, repoPath string, sel protocol.RunnerSelector) (*agentexec.CommandExecutionStream, string, error) {
	return c.OpenInteractiveWithSelectorAndArgs(ctx, repoPath, sel, nil, "")
}

// OpenInteractiveWithSelectorAndArgs is the full-featured form: selector
// pinning, per-task extraArgs (forwarded verbatim), and an optional
// resumeTaskID hex string that re-uses an existing terminal interactive task
// id and worktree branch.
func (c *Client) OpenInteractiveWithSelectorAndArgs(ctx context.Context, repoPath string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string) (*agentexec.CommandExecutionStream, string, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repoPath))
	oi.Selector = sel
	oi.ExtraArgs = protocol.ClaudeArgsFromStrings(extraArgs)
	if resumeTaskID != "" {
		tid, err := parseTaskIDHex(resumeTaskID)
		if err != nil {
			return nil, "", fmt.Errorf("OpenInteractive: parse resume id: %w", err)
		}
		oi.ResumeTaskId = tid
	}
	req.SetOpenInteractive(oi)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, "", err
	}
	if resp.Kind != protocol.TaskControlKind_OpenInteractive {
		return nil, "", fmt.Errorf("expected OpenInteractive response, got kind=%v", resp.Kind)
	}
	oir := resp.OpenInteractive()
	if oir == nil {
		return nil, "", fmt.Errorf("OpenInteractive response variant missing")
	}
	if err := openInteractiveStatusError(repoPath, oir.Status); err != nil {
		return nil, "", err
	}

	taskIDHex := hex.EncodeToString(oir.TaskId.Id[:])

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(oir.StreamId))
	if st == nil {
		return nil, taskIDHex, fmt.Errorf("exec stream %d not visible after OpenInteractive", oir.StreamId)
	}
	return agentexec.NewCommandExecutionStream(st), taskIDHex, nil
}

// openInteractiveStatusError converts a non-Ok OpenInteractiveStatus into a
// Go error. Returns nil for OpenInteractiveStatus_Ok.
func openInteractiveStatusError(repo string, status protocol.OpenInteractiveStatus) error {
	switch status {
	case protocol.OpenInteractiveStatus_Ok:
		return nil
	case protocol.OpenInteractiveStatus_NoRunnerForRepo:
		return fmt.Errorf("interactive no_runner_for_repo: no idle runner for repo %q", repo)
	case protocol.OpenInteractiveStatus_RunnerBusy:
		return fmt.Errorf("interactive runner_busy: runner is at capacity")
	case protocol.OpenInteractiveStatus_AmbiguousRunner:
		return fmt.Errorf("interactive ambiguous_runner: multiple runners match; pin one with --runner/--host/--ip")
	case protocol.OpenInteractiveStatus_PinnedNotFound:
		return fmt.Errorf("interactive pinned_not_found: the specified runner was not found")
	case protocol.OpenInteractiveStatus_ResumeNotFound:
		return fmt.Errorf("interactive resume_not_found: the specified resume task id is unknown (was it pruned, or is the kind a mismatch?)")
	case protocol.OpenInteractiveStatus_ResumeNotTerminal:
		return fmt.Errorf("interactive resume_not_terminal: the resume target is still queued/running (or another resume is already in flight)")
	case protocol.OpenInteractiveStatus_InternalError:
		return fmt.Errorf("interactive internal_error")
	default:
		return fmt.Errorf("interactive error (status=%d)", status)
	}
}

// Interactive splices stdin/stdout/SIGWINCH between the local terminal and
// the remote PTY for an interactive claude session in repo. Method form:
// callable on an existing *Client without re-dialing. The caller's terminal
// must be a real tty (RemoteShell flips it into raw mode). Returns the new
// task's hex id even on error so the caller can surface it for cleanup.
func (c *Client) Interactive(ctx context.Context, repo string) (string, error) {
	return c.InteractiveWithSelectorAndArgs(ctx, repo, protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, "")
}

// InteractiveWithSelector is the same as Interactive but accepts an explicit
// runner selector. extraArgs default to none.
func (c *Client) InteractiveWithSelector(ctx context.Context, repo string, sel protocol.RunnerSelector) (string, error) {
	return c.InteractiveWithSelectorAndArgs(ctx, repo, sel, nil, "")
}

// InteractiveWithSelectorAndArgs is the full-featured form: selector pinning,
// per-task extraArgs, and optional resumeTaskID (hex) for reusing an
// existing terminal interactive task.
func (c *Client) InteractiveWithSelectorAndArgs(ctx context.Context, repo string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string) (string, error) {
	stream, taskIDHex, err := c.OpenInteractiveWithSelectorAndArgs(ctx, repo, sel, extraArgs, resumeTaskID)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	// stderr because stdout is owned by the remote PTY's output once
	// RemoteShell starts. Printing before MakeRaw keeps the message in
	// cooked mode so the trailing newline behaves.
	fmt.Fprintf(os.Stderr, "harness-cli: attached to task %s (Ctrl+D / `exit` to detach)\n", taskIDHex)

	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}

// Interactive (package-level) is a thin wrapper that opens a fresh Client per
// call: dial, open the interactive session, splice, then close. Suitable for
// the harness-cli `interactive` subcommand. Long-lived consumers should hold
// a *Client and call (*Client).Interactive instead.
func Interactive(ctx context.Context, peerCID objproto.ConnectionID, repo string) (string, error) {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Interactive(ctx, repo)
}
