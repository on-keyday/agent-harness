//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"

	agentexec "github.com/on-keyday/agent-harness/exec"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// AttachSession re-attaches to an existing detachable interactive session
// identified by taskIDHex (32 lowercase hex chars). Returns the
// CommandExecutionStream wired to the client end, and replayBytes indicating
// how many bytes of scrollback the server will replay from the beginning of
// the stream.
//
// On success the caller owns the returned stream and is responsible for
// calling RemoteShell (or Stdin/Stdout/Stderr individually) and Close.
func (c *Client) AttachSession(ctx context.Context, taskIDHex string) (*agentexec.CommandExecutionStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("AttachSession: parse task id: %w", err)
	}

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AttachSession}
	req.SetAttach(protocol.AttachSessionRequest{TaskId: tid})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if resp.Kind != protocol.TaskControlKind_AttachSession {
		return nil, 0, fmt.Errorf("expected AttachSession response, got kind=%v", resp.Kind)
	}
	ar := resp.Attach()
	if ar == nil {
		return nil, 0, fmt.Errorf("AttachSession response variant missing")
	}
	if err := attachStatusError(taskIDHex, ar.Status); err != nil {
		return nil, 0, err
	}

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(ar.StreamId))
	if st == nil {
		return nil, ar.ReplayBytes, fmt.Errorf("exec stream %d not visible after AttachSession", ar.StreamId)
	}
	return agentexec.NewCommandExecutionStream(st), ar.ReplayBytes, nil
}

// SessionAttach is the high-level helper: it calls AttachSession, prints a
// short informational line to stderr (replay bytes), then runs RemoteShell to
// splice the local terminal to the remote PTY. Returns the task's hex id even
// on error so the caller can surface it.
func (c *Client) SessionAttach(ctx context.Context, taskIDHex string) (string, error) {
	stream, replayBytes, err := c.AttachSession(ctx, taskIDHex)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	// stderr: stdout is owned by the remote PTY once RemoteShell starts.
	fmt.Fprintf(os.Stderr, "harness-cli: attached to task %s (replay %d bytes; Ctrl+D / `exit` to detach)\n", taskIDHex, replayBytes)

	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}

// attachStatusError converts a non-Ok AttachSessionStatus into a Go error.
// Returns nil for AttachSessionStatus_Ok.
func attachStatusError(taskID string, status protocol.AttachSessionStatus) error {
	switch status {
	case protocol.AttachSessionStatus_Ok:
		return nil
	case protocol.AttachSessionStatus_NotFound:
		return fmt.Errorf("attach not_found: task %q not found (pruned, or wrong id?)", taskID)
	case protocol.AttachSessionStatus_NotInteractive:
		return fmt.Errorf("attach not_interactive: task %q is not an interactive session", taskID)
	case protocol.AttachSessionStatus_NotDetachable:
		return fmt.Errorf("attach not_detachable: task %q was not started as a detachable session", taskID)
	case protocol.AttachSessionStatus_AlreadyTerminal:
		return fmt.Errorf("attach already_terminal: task %q has already finished", taskID)
	case protocol.AttachSessionStatus_RunnerUnreachable:
		return fmt.Errorf("attach runner_unreachable: the runner hosting task %q is not connected", taskID)
	case protocol.AttachSessionStatus_InternalError:
		return fmt.Errorf("attach internal_error: server error while attaching to task %q", taskID)
	default:
		return fmt.Errorf("attach error (status=%d) for task %q", status, taskID)
	}
}
