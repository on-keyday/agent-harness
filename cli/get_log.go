package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// GetTaskLog fetches the historical log for `taskIDHex` from the server.
// The server replies with a TaskControlResponse{GetTaskLog} that points at a
// server-initiated send-stream; this helper reads the stream until EOF and
// returns the assembled bytes.
//
// Method form: callable on an existing *Client without re-dialing.
//
// Returns (nil, false, nil) when the server has no log file for the task
// (e.g. tasks pruned, or DataDir-less server).
func (c *Client) GetTaskLog(ctx context.Context, taskIDHex string) ([]byte, bool, error) {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return nil, false, fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return nil, false, fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_GetTaskLog}
	req.SetGetLog(protocol.GetTaskLogRequest{TaskId: tid})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, false, err
	}
	gl := resp.GetLog()
	if gl == nil || resp.Kind != protocol.TaskControlKind_GetTaskLog {
		return nil, false, fmt.Errorf("expected GetTaskLog response, got kind=%v", resp.Kind)
	}
	if gl.Found == 0 {
		return nil, false, nil
	}

	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(gl.StreamId))
	if st == nil {
		return nil, true, fmt.Errorf("stream %d not visible after GetTaskLog response", gl.StreamId)
	}
	var out []byte
	for {
		select {
		case <-ctx.Done():
			return nil, true, ctx.Err()
		default:
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, true, err
		}
		if len(data) > 0 {
			out = append(out, data...)
		}
		if eof {
			return out, true, nil
		}
	}
}

// GetTaskLog (package-level) is a thin wrapper that opens a fresh Client per
// call. Suitable for short-lived CLI processes. Long-lived consumers should
// hold a *Client and call (*Client).GetTaskLog instead.
func GetTaskLog(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string) ([]byte, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, false, err
	}
	defer c.Close()
	return c.GetTaskLog(ctx, taskIDHex)
}

// waitForReceiveStream polls Transport.GetReceiveStream until the stream
// becomes visible or ctx / 2s deadline elapses. The server-initiated
// send-stream may not be visible to the client yet when the GetTaskLog
// response arrives — the response goes through objproto.SendMessage while
// the stream-creation trsf frame travels via AutoSend.
func waitForReceiveStream(ctx context.Context, p trsf.Transport, id trsf.StreamID) trsf.ReceiveStream {
	if st := p.GetReceiveStream(id); st != nil {
		return st
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-tick.C:
			if st := p.GetReceiveStream(id); st != nil {
				return st
			}
		}
	}
}
