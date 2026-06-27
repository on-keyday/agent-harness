//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"

	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AttachSession re-attaches to an existing detachable interactive session
// identified by taskIDHex (32 lowercase hex chars). Returns the
// CommandExecutionStream wired to the client end, and replayBytes indicating
// how many bytes of scrollback the server will replay from the beginning of
// the stream.
//
// On success the caller owns the returned stream and is responsible for
// calling RemoteShell (or Stdin/Stdout/Stderr individually) and Close.
func (c *Client) AttachSession(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (*agentexec.CommandExecutionStream, uint64, error) {
	st, replayBytes, err := c.attachSessionRPC(ctx, taskIDHex, mode)
	if err != nil {
		return nil, 0, err
	}
	return agentexec.NewCommandExecutionStream(st), replayBytes, nil
}

// SessionAttach is the high-level helper: it calls AttachSession, prints a
// short informational line to stderr (replay bytes), then runs RemoteShell to
// splice the local terminal to the remote PTY. Returns the task's hex id even
// on error so the caller can surface it.
func (c *Client) SessionAttach(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (string, error) {
	stream, replayBytes, err := c.AttachSession(ctx, taskIDHex, mode)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	// stderr: stdout is owned by the remote PTY once RemoteShell starts.
	if mode == protocol.AttachMode_View {
		fmt.Fprintf(os.Stderr, "harness-cli: VIEW-ONLY attach to task %s (replay %d bytes; your input is ignored; Ctrl+] to detach)\n", taskIDHex, replayBytes)
	} else {
		fmt.Fprintf(os.Stderr, "harness-cli: attached to task %s (replay %d bytes; Ctrl+] to detach client; Ctrl+D / `exit` ends the session)\n", taskIDHex, replayBytes)
	}

	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}
