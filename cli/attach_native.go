//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// AttachSession re-attaches to an existing detachable session identified by
// taskIDHex (32 lowercase hex chars). Returns the CommandExecutionStream wired
// to the client end, replayBytes indicating how many bytes of scrollback the
// server will replay from the beginning of the stream, and the task's
// TaskKind — which says whether those frames carry terminal bytes
// (interactive) or neutral NDJSON (stream). Every caller must decide what it
// does with the kind; a caller that renders one payload as the other shows
// the operator garbage.
//
// On success the caller owns the returned stream and is responsible for
// calling RemoteShell (or Stdin/Stdout/Stderr individually) and Close.
func (c *Client) AttachSession(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (*agentexec.CommandExecutionStream, uint64, protocol.TaskKind, error) {
	return c.AttachSessionWithReplayLimit(ctx, taskIDHex, mode, 0)
}

// AttachSessionWithReplayLimit is AttachSession with a replay cap (bytes; 0 =
// full ring). Only observer attaches (view/cowrite) honor the cap server-side;
// a control reattach always replays the full ring. Used by the monitoring grid
// so a pane isn't shipped scrollback it will never render.
func (c *Client) AttachSessionWithReplayLimit(ctx context.Context, taskIDHex string, mode protocol.AttachMode, replayLimit uint32) (*agentexec.CommandExecutionStream, uint64, protocol.TaskKind, error) {
	st, ar, err := c.attachSessionRPC(ctx, taskIDHex, mode, replayLimit)
	if err != nil {
		return nil, 0, 0, err
	}
	return agentexec.NewCommandExecutionStream(st), ar.ReplayBytes, ar.Kind, nil
}

// SessionAttach is the high-level helper: it calls AttachSession, prints a
// short informational line to stderr (replay bytes), then runs RemoteShell to
// splice the local terminal to the remote PTY. Returns the task's hex id even
// on error so the caller can surface it.
//
// It refuses an event-stream task: RemoteShell puts the local terminal in raw
// mode and paints the frames as terminal bytes, which for that kind is raw
// NDJSON. The stream verb renders it instead.
func (c *Client) SessionAttach(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (string, error) {
	stream, replayBytes, kind, err := c.AttachSession(ctx, taskIDHex, mode)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	if kind == protocol.TaskKind_Stream {
		return taskIDHex, fmt.Errorf("task %s is an event-stream session (structured events, no terminal): use `session stream attach %s`: %w", taskIDHex, taskIDHex, ErrAttachWrongKind)
	}

	// stderr: stdout is owned by the remote PTY once RemoteShell starts.
	if mode == protocol.AttachMode_View {
		fmt.Fprintf(os.Stderr, "harness-cli: VIEW-ONLY attach to task %s (replay %d bytes; your input is ignored; Ctrl+] to detach)\n", taskIDHex, replayBytes)
	} else {
		fmt.Fprintf(os.Stderr, "harness-cli: attached to task %s (replay %d bytes; Ctrl+] to detach client; Ctrl+D / `exit` ends the session)\n", taskIDHex, replayBytes)
	}

	// RemoteShell returns when the operator detaches (Ctrl+]) or the session
	// ends, with the local terminal already back in cooked mode and its screen
	// and input modes reset — it writes both reset groups on the way out,
	// whether it is returning cleanly or not (objtrsf exec.WriteTerminalReset).
	err = stream.RemoteShell()
	if err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}
