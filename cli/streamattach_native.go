//go:build !js

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// SessionStreamAttach follows an event-stream task: it view-attaches, decodes
// the neutral NDJSON riding in the Stdout frames, and renders each message as
// the same one-line text the task log carries (streamagent.RenderText — the
// shared renderer is what keeps `logs`, the runner tap and this view from
// drifting apart). Rendered lines go to out; the agent's own stderr and this
// function's informational lines go to errOut.
//
// A VIEW attach on purpose: following is exec_view's meaning for this kind,
// several followers may coexist, and no seat is taken — the task stays
// Detached, which for this kind is the ordinary state, not an anomaly.
// Sending (turns, approvals) is the write verbs' job, not this one's.
//
// Returns when the stream ends: the task finished, the server dropped this
// observer for falling behind, or ctx was cancelled. Detaching (Ctrl+C on the
// CLI) never affects the task.
func (c *Client) SessionStreamAttach(ctx context.Context, taskIDHex string, out, errOut io.Writer) error {
	stream, replayBytes, kind, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return err
	}
	defer stream.Close()

	if kind != protocol.TaskKind_Stream {
		return fmt.Errorf("task %s is not an event-stream session (kind %s): use `session attach %s`: %w",
			taskIDHex, kind, taskIDHex, ErrAttachWrongKind)
	}

	fmt.Fprintf(errOut, "harness-cli: following events of task %s (replay %d bytes; Ctrl+C detaches, the task keeps running)\n", taskIDHex, replayBytes)

	// The agent's stderr rides its own frame type and is not NDJSON; copy it
	// through verbatim, concurrently — an undrained demux side backpressures
	// the stream.
	go func() { _, _ = io.Copy(errOut, stream.Stderr()) }()

	r := bufio.NewReader(stream.Stdout())
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			renderStreamLine(line, out, errOut)
		}
		if rerr != nil {
			if rerr == io.EOF {
				fmt.Fprintf(errOut, "harness-cli: stream ended (task finished, or this observer was dropped)\n")
				return nil
			}
			if ctx.Err() != nil {
				return nil // detached by the caller, not a failure
			}
			return rerr
		}
	}
}

// renderStreamLine renders one NDJSON line for the follow view. Events and
// requests use the shared renderer; hello and exit get follow-specific wording
// (the tap words its own); anything else — including a line that is not the
// protocol at all, which `session send` can lawfully put on the stream — is
// shown raw and marked, never dropped: a follower who cannot see what a
// cowriter injected cannot explain what the adapter does next.
func renderStreamLine(line []byte, out, errOut io.Writer) {
	m, err := streamagent.DecodeMsg(line)
	if err != nil {
		fmt.Fprintf(out, "(not the protocol) %s\n", line)
		return
	}
	if text, ok := streamagent.RenderText(m); ok {
		fmt.Fprintln(out, text)
		return
	}
	switch m.Kind {
	case streamagent.KindHello:
		if m.Hello != nil {
			fmt.Fprintf(errOut, "harness-cli: adapter hello: vendor=%s protocol=%d capabilities=%v\n",
				m.Hello.Vendor, m.Hello.Protocol, m.Hello.Capabilities)
		}
	case streamagent.KindExit:
		if m.Exit != nil {
			if m.Exit.Err != "" {
				fmt.Fprintf(out, "agent exited: code=%d err=%s\n", m.Exit.Code, m.Exit.Err)
			} else {
				fmt.Fprintf(out, "agent exited: code=%d\n", m.Exit.Code)
			}
		}
	}
	// Client→adapter kinds (response/user/interrupt/finish) do not appear on
	// this direction of the stream; nothing to render.
}
