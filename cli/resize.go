package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// resizeEchoPoll is how often SessionResize re-reads the stream's last-seen
// window size while waiting for the server to echo its own frame back.
const resizeEchoPoll = 25 * time.Millisecond

// SessionResize sets a detachable session's PTY size without taking it over,
// and REPORTS whether the size actually took.
//
// The reporting is the point. A resize from a non-control attach is honoured
// only when the caller holds exec_resize AND no control client holds the seat
// (see SessionMux.applyObserverWinSize); otherwise the server discards the
// frame, which is right for the implicit per-SIGWINCH stream a real terminal
// emits but wrong for someone who typed `session resize`. A typed option must
// take effect or say it did not.
//
// The confirmation needs no new message: an accepted size is fanned out to
// every observer, INCLUDING the one that sent it, so the caller's own stream
// sees its size come back. Waiting for that echo is the acknowledgement. The
// server also replays the CURRENT size ahead of the ring at attach, so the wait
// is for the size to become the requested one, not merely for any size to
// arrive.
//
// A session that already has exactly this size echoes nothing to distinguish —
// and reports applied, which is the truthful answer to "make it 40x150".
//
// Untagged like snapshot_raw.go: it drives CommandExecutionStream directly and
// so serves the js build too.
func (c *Client) SessionResize(ctx context.Context, taskIDHex string, rows, cols uint16, wait time.Duration) (applied bool, err error) {
	if rows == 0 || cols == 0 {
		return false, fmt.Errorf("resize: rows and cols must both be non-zero (got %dx%d)", rows, cols)
	}
	// A VIEW attach is enough: resizing is orthogonal to the mode, so asking
	// for cowrite here would demand a strictly larger grant than the operation
	// needs. exec_view + exec_resize is the whole requirement.
	st, ar, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View, 0)
	if err != nil {
		return false, err
	}
	stream := agentexec.NewCommandExecutionStream(st)
	defer stream.Close()

	// §3 of the event-stream design: resize is REFUSED for that kind, never a
	// silent no-op. Without this check the window-size frame would be sent,
	// never echoed (a stream session emits none), and the caller would get the
	// misleading "needs exec_resize" hint after the full wait.
	if ar.Kind == protocol.TaskKind_Stream {
		return false, fmt.Errorf("task %s is an event-stream session: it has no PTY to resize: %w", taskIDHex, ErrAttachWrongKind)
	}

	// Drain in the background: the replay burst has to be consumed for the
	// frame demux to reach our echo, and leaving the recv side undrained
	// backpressures the stream (project_trsf_accept_queue_wedge).
	go func() {
		buf := make([]byte, 32*1024)
		out := stream.Stdout()
		for {
			if _, rerr := out.Read(buf); rerr != nil {
				return
			}
		}
	}()

	// width/height in pixels are 0: the harness sizes in cells only, and every
	// other sender in this repo does the same.
	if err := stream.SetTerminalWindowSize(rows, cols, 0, 0); err != nil {
		return false, err
	}

	deadline := time.Now().Add(wait)
	for {
		if gotRows, gotCols, ok := stream.LastWindowSize(); ok && gotRows == rows && gotCols == cols {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-time.After(resizeEchoPoll):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// ResizeRejectedHint is the operator-facing explanation for an unapplied
// resize. There are exactly two reasons and the client cannot tell them apart
// — the server discards the frame silently either way — so it names both
// rather than guessing.
const ResizeRejectedHint = "not applied: resizing from a non-control attach needs the exec_resize capability, " +
	"and applies only while no control client is attached (the control attach owns the size whenever it holds the seat)"
