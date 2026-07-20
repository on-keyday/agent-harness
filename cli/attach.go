package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// ErrAttachNotFound / ErrAttachTerminal mark AttachSession failures that can
// never succeed on retry: the task is gone (pruned/unknown) or has already
// finished. A monitoring caller that reattaches on drop (the TUI grid) uses
// IsAttachPermanent to stop retrying these, while treating every other error
// (runner briefly unreachable, wire hiccup, a fell-behind observer dropped by
// the server) as transient and worth another attach.
var (
	ErrAttachNotFound = errors.New("attach: task not found")
	ErrAttachTerminal = errors.New("attach: task already finished")
)

// IsAttachPermanent reports whether an AttachSession error will never succeed on
// retry, so a reattaching caller should give up rather than back off and try
// again.
func IsAttachPermanent(err error) bool {
	return errors.Is(err, ErrAttachNotFound) || errors.Is(err, ErrAttachTerminal)
}

// attachSessionRPC performs the AttachSession RPC round-trip and returns the
// raw bidirectional stream plus the server-reported replayBytes count.
// It is shared between native (cli/attach_native.go) and WASM
// (cli/attach_js.go) callers; neither syscall nor exec dependencies are
// introduced here.
// replayLimit caps the replay the server sends back (0 = full ring); only
// observer attaches (view/cowrite) honor it. A monitoring grid pane passes a
// small limit so it isn't shipped ~1 MiB of scrollback it will never show.
func (c *Client) attachSessionRPC(ctx context.Context, taskIDHex string, mode protocol.AttachMode, replayLimit uint32) (trsf.BidirectionalStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("AttachSession: parse task id: %w", err)
	}

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AttachSession}
	req.SetAttach(protocol.AttachSessionRequest{TaskId: tid, Mode: mode, ReplayLimit: replayLimit})

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
	return st, ar.ReplayBytes, nil
}

// attachStatusError converts a non-Ok AttachSessionStatus into a Go error.
// Returns nil for AttachSessionStatus_Ok.
func attachStatusError(taskID string, status protocol.AttachSessionStatus) error {
	switch status {
	case protocol.AttachSessionStatus_Ok:
		return nil
	case protocol.AttachSessionStatus_NotFound:
		return fmt.Errorf("attach not_found: task %q not found (pruned, or wrong id?): %w", taskID, ErrAttachNotFound)
	case protocol.AttachSessionStatus_NotInteractive:
		return fmt.Errorf("attach not_interactive: task %q is not an interactive session", taskID)
	case protocol.AttachSessionStatus_AlreadyTerminal:
		return fmt.Errorf("attach already_terminal: task %q has already finished: %w", taskID, ErrAttachTerminal)
	case protocol.AttachSessionStatus_RunnerUnreachable:
		return fmt.Errorf("attach runner_unreachable: the runner hosting task %q is not connected", taskID)
	case protocol.AttachSessionStatus_InternalError:
		return fmt.Errorf("attach internal_error: server error while attaching to task %q", taskID)
	default:
		return fmt.Errorf("attach error (status=%d) for task %q", status, taskID)
	}
}
