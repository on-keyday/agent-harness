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

// ForwardTapOpts are the knobs `forward tap` exposes.
type ForwardTapOpts struct {
	Filter         protocol.ForwardTapFilter
	MaxRecordBytes uint32
	Mode           TapRenderMode
}

// OpenForwardTapWith attaches a tap to forwardID over an existing client and
// returns the stream its records arrive on.
//
// A method on the long-lived *Client, like OpenPortForward: the TUI and the
// WebUI both already hold one, and dialling afresh there would throw away a
// handshake.
func (c *Client) OpenForwardTap(ctx context.Context, forwardID uint64, opts ForwardTapOpts) (trsf.BidirectionalStream, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenForwardTap}
	req.SetOpenForwardTap(protocol.OpenForwardTapRequest{
		ForwardId:       forwardID,
		DirectionFilter: opts.Filter,
		MaxRecordBytes:  opts.MaxRecordBytes,
	})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		// A caller without the capability lands here: RoundTripTaskControl
		// recognises PermissionDenied at one point and turns it into a
		// CapabilityError naming forward_tap, so this verb needs no gate of its
		// own.
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenForwardTap {
		return nil, fmt.Errorf("forward tap: unexpected response kind %v", resp.Kind)
	}
	r := resp.OpenForwardTap()
	if r == nil {
		return nil, errors.New("forward tap: response variant missing")
	}
	switch r.Status {
	case protocol.OpenForwardTapStatus_Ok:
	case protocol.OpenForwardTapStatus_NoSuchForward:
		return nil, fmt.Errorf("forward tap: no such forward %d (unknown, or not visible to you)", forwardID)
	default:
		return nil, fmt.Errorf("forward tap: server error (status=%v)", r.Status)
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("forward tap: stream %d not visible", r.StreamId)
	}
	return st, nil
}

// tapRecordReader turns the stream's byte chunks back into records.
//
// The stream is a CONCATENATION of self-delimiting records with no length
// prefix — a prefix would be a wire byte the schema does not describe — so a
// chunk can end mid-record and a chunk can hold several. push accumulates and
// decodes as far as it can, keeping the remainder.
type tapRecordReader struct {
	buf []byte
}

func (r *tapRecordReader) push(chunk []byte) ([]*protocol.ForwardTapRecord, error) {
	r.buf = append(r.buf, chunk...)
	var out []*protocol.ForwardTapRecord
	for len(r.buf) > 0 {
		rec := &protocol.ForwardTapRecord{}
		rest, err := rec.Decode(r.buf)
		if err != nil {
			// Short read: wait for more bytes. There is no way to tell a
			// truncated record from a malformed one here, and treating an
			// incomplete tail as an error would break every split chunk.
			break
		}
		out = append(out, rec)
		if len(rest) == len(r.buf) {
			// No progress: refuse to spin.
			return out, errors.New("forward tap: decoder made no progress")
		}
		r.buf = rest
	}
	return out, nil
}

// RunForwardTap streams a tap to w until the forward ends, the context is
// cancelled, or the stream fails. It is the whole body of `harness-cli forward
// tap`, and the TUI/WebUI pumps use OpenForwardTap + tapRecordReader directly
// so they can render into their own surfaces.
func RunForwardTap(ctx context.Context, c *Client, forwardID uint64, opts ForwardTapOpts, w io.Writer) error {
	st, err := c.OpenForwardTap(ctx, forwardID, opts)
	if err != nil {
		return err
	}
	defer func() { _ = st.CloseBoth() }()

	var reader tapRecordReader
	for {
		data, eof, rerr := st.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			recs, derr := reader.push(data)
			if derr != nil {
				return derr
			}
			for _, rec := range recs {
				for _, line := range RenderTapRecord(rec, opts.Mode) {
					if opts.Mode == TapRaw {
						if _, werr := io.WriteString(w, line); werr != nil {
							return werr
						}
						continue
					}
					if _, werr := fmt.Fprintln(w, line); werr != nil {
						return werr
					}
				}
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil // the operator stopped it; not a failure
			}
			return rerr
		}
		if eof {
			return nil
		}
	}
}

// RunForwardTapDial is the harness-cli entry point: it dials, taps, and streams
// until the forward ends or the operator interrupts. The *With form above is
// what the TUI and the WebUI use, on the client they already hold.
func RunForwardTapDial(ctx context.Context, peerCID objproto.ConnectionID, forwardID uint64, opts ForwardTapOpts, w io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return RunForwardTap(ctx, c, forwardID, opts, w)
}

// StreamForwardTap taps forwardID and hands each record's rendered lines to
// onLines until the forward ends or ctx is cancelled.
//
// It is RunForwardTap with a callback instead of an io.Writer, for the two
// surfaces that draw into something other than a file: the TUI's view and the
// browser's panel. Rendering stays in RenderTapRecord so all three print the
// same text.
func StreamForwardTap(ctx context.Context, c *Client, forwardID uint64, opts ForwardTapOpts, onLines func([]string)) error {
	st, err := c.OpenForwardTap(ctx, forwardID, opts)
	if err != nil {
		return err
	}
	defer func() { _ = st.CloseBoth() }()

	var reader tapRecordReader
	for {
		data, eof, rerr := st.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			recs, derr := reader.push(data)
			if derr != nil {
				return derr
			}
			var lines []string
			for _, rec := range recs {
				lines = append(lines, RenderTapRecord(rec, opts.Mode)...)
			}
			if len(lines) > 0 && onLines != nil {
				onLines(lines)
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return rerr
		}
		if eof {
			return nil
		}
	}
}
