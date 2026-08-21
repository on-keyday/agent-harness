//go:build js

package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AttachSession (WASM) re-attaches to an existing detachable interactive
// session identified by taskIDHex. It performs the AttachSession RPC, acquires
// the bidirectional stream, and installs it as the singleton
// activeInteractiveSession — exactly like InteractiveWithSelectorAndArgs does
// for a fresh session. The browser xterm will receive replayed + live output
// via harness_xtermWrite without any additional wiring.
//
// Installation, the recv pump, the single-writer generation guard, and the
// detach-and-drain of any previous session are all handled by
// installAndPumpSession (see open_interactive_wasm.go) — shared verbatim with
// the fresh-session path so the two cannot drift.
//
// Returns the task's hex id (same as taskIDHex) on success.
func (c *Client) AttachSession(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (string, error) {
	// Mark the attach as in flight BEFORE the RPC: when this browser is
	// already the controlling client of taskIDHex, the server closes that old
	// stream while this RPC is still running, and the old recv pump must not
	// report the closure as a foreign takeover (see pendingAttachTask in
	// open_interactive_wasm.go). Cleared on every exit path; an attach that
	// fails leaves the JS error path in charge of the indicator.
	setPendingAttach(taskIDHex)
	defer clearPendingAttach()

	stream, ar, err := c.attachSessionRPC(ctx, taskIDHex, mode, 0)
	if err != nil {
		return "", err
	}
	// The singleton this installs feeds the browser XTERM, which would paint
	// an event-stream task's frames as raw NDJSON. Refuse with the kind the
	// server just reported; the logs pane already renders that task's events.
	if ar.Kind == protocol.TaskKind_Stream {
		stream.CloseBoth()
		return "", fmt.Errorf("task %s is an event-stream session (structured events, no terminal): follow it in the logs pane, or `session stream attach` from the CLI: %w", taskIDHex, ErrAttachWrongKind)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	session := &InteractiveSession{
		stream:    stream,
		taskIDHex: taskIDHex,
		ctx:       sessCtx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	installAndPumpSession(session)

	return taskIDHex, nil
}
