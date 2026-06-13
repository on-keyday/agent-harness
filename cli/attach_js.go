//go:build js

package cli

import (
	"context"

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
	stream, _, err := c.attachSessionRPC(ctx, taskIDHex, mode)
	if err != nil {
		return "", err
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
