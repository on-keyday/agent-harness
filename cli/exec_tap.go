package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// ExecTapOpts are the knobs `exec tap` exposes.
type ExecTapOpts struct {
	Filter         protocol.ExecTapFilter
	MaxRecordBytes uint32
	Mode           TapRenderMode
}

// OpenExecTap attaches a tap to execID over an existing client and returns the
// stream its records arrive on. A method on the long-lived *Client for
// OpenForwardTap's reason: the TUI and the WebUI already hold one.
func (c *Client) OpenExecTap(ctx context.Context, execID uint64, opts ExecTapOpts) (trsf.BidirectionalStream, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenExecTap}
	req.SetOpenExecTap(protocol.OpenExecTapRequest{
		ExecId:         execID,
		ChannelFilter:  opts.Filter,
		MaxRecordBytes: opts.MaxRecordBytes,
	})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		// A caller without exec_tap lands here as a CapabilityError naming the
		// bit; RoundTripTaskControl converts PermissionDenied at one point.
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenExecTap {
		return nil, fmt.Errorf("exec tap: unexpected response kind %v", resp.Kind)
	}
	r := resp.OpenExecTap()
	if r == nil {
		return nil, errors.New("exec tap: response variant missing")
	}
	switch r.Status {
	case protocol.OpenExecTapStatus_Ok:
	case protocol.OpenExecTapStatus_NoSuchExec:
		return nil, fmt.Errorf("exec tap: no such exec %d (unknown, ended, or not visible to you)", execID)
	default:
		return nil, fmt.Errorf("exec tap: server error (status=%v)", r.Status)
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("exec tap: stream %d not visible", r.StreamId)
	}
	return st, nil
}

// RunExecTap streams a tap to w until the exec ends, ctx is cancelled, or the
// stream fails. The whole body of `harness-cli exec tap`.
func RunExecTap(ctx context.Context, c *Client, execID uint64, opts ExecTapOpts, w io.Writer) error {
	st, err := c.OpenExecTap(ctx, execID, opts)
	if err != nil {
		return err
	}
	defer func() { _ = st.CloseBoth() }()
	return streamTapRecords(ctx, st, func(recs []*protocol.ExecTapRecord) error {
		for _, rec := range recs {
			if err := writeTapLines(w, RenderExecTapRecord(execID, rec, opts.Mode), opts.Mode); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunExecTapDial is the harness-cli entry point: dial, tap, stream.
func RunExecTapDial(ctx context.Context, peerCID objproto.ConnectionID, execID uint64, opts ExecTapOpts, w io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return RunExecTap(ctx, c, execID, opts, w)
}

// StreamExecTap is RunExecTap with a callback, for the TUI view and the
// browser panel. Rendering stays in RenderExecTapRecord so all three surfaces
// print the same text.
func StreamExecTap(ctx context.Context, c *Client, execID uint64, opts ExecTapOpts, onLines func([]string)) error {
	st, err := c.OpenExecTap(ctx, execID, opts)
	if err != nil {
		return err
	}
	defer func() { _ = st.CloseBoth() }()
	return streamTapRecords(ctx, st, func(recs []*protocol.ExecTapRecord) error {
		var lines []string
		for _, rec := range recs {
			lines = append(lines, RenderExecTapRecord(execID, rec, opts.Mode)...)
		}
		if len(lines) > 0 && onLines != nil {
			onLines(lines)
		}
		return nil
	})
}
