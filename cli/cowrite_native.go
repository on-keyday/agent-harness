//go:build !js

package cli

import (
	"context"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SessionSend injects input into a detachable interactive session via a
// co-writer attach (AttachMode_Cowrite): the keystrokes are forwarded to the
// session's PTY WITHOUT taking over the controlling client and WITHOUT changing
// the PTY size (a cowriter has no size authority). Pair with SessionSnapshot to
// drive a session statelessly from a non-TTY context: send, then snapshot.
//
// flush is how long to let the input drain to the runner before detaching; the
// stream Close cancels the underlying transport, so closing immediately after
// the write can drop the in-flight frame.
func (c *Client) SessionSend(ctx context.Context, taskIDHex string, data []byte, flush time.Duration) error {
	stream, _, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_Cowrite)
	if err != nil {
		return err
	}
	defer stream.Close()
	if len(data) > 0 {
		if _, err := stream.Stdin().Write(data); err != nil {
			return err
		}
	}
	if flush > 0 {
		t := time.NewTimer(flush)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return nil
}
