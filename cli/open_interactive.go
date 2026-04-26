package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	agentexec "github.com/on-keyday/agent-harness/exec"
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
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repoPath))
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
	switch oir.Status {
	case 0: // ok
	case 1:
		return nil, "", fmt.Errorf("no idle runner for repo %q", repoPath)
	case 2:
		return nil, "", fmt.Errorf("runner busy")
	default:
		return nil, "", fmt.Errorf("server-side error opening interactive (status=%d)", oir.Status)
	}

	taskIDHex := hex.EncodeToString(oir.TaskId.Id[:])

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(oir.StreamId))
	if st == nil {
		return nil, taskIDHex, fmt.Errorf("exec stream %d not visible after OpenInteractive", oir.StreamId)
	}
	return agentexec.NewCommandExecutionStream(st), taskIDHex, nil
}

// Interactive is the one-shot helper for harness-cli's `interactive`
// subcommand: dial, open the interactive session, RemoteShell to splice
// stdin/stdout/SIGWINCH between the local terminal and the remote PTY,
// then close. The caller's terminal must be a real tty (RemoteShell
// flips it into raw mode). Returns the new task's hex id even on error
// so the caller can surface it for cleanup.
func Interactive(ctx context.Context, addr, repo string) (string, error) {
	c, err := Dial(ctx, addr)
	if err != nil {
		return "", err
	}
	defer c.Close()

	stream, taskIDHex, err := c.OpenInteractive(ctx, repo)
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
