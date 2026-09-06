package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// Errors a trsf_state refusal turns into, so a caller can tell a stale runner
// id from a confined view from a runner that simply did not answer.
var (
	ErrTrsfRunnerOffline = errors.New("trsf: no such runner is registered")
	ErrTrsfNotPermitted  = errors.New("trsf: reading a runner's transport state needs the global view (a confined caller sees no runner connections)")
	ErrTrsfUnavailable   = errors.New("trsf: the runner did not answer")
)

// TrsfStateOn reads congestion state from one end of the fleet.
//
// runnerCID empty asks the SERVER about its own connections; otherwise the
// server asks that runner about its. The two are different machines and the
// answer says which by the role on each row.
func (c *Client) TrsfStateOn(ctx context.Context, runnerCID string) ([]protocol.TrsfConnState, error) {
	body := protocol.TrsfStateRequest{Target: protocol.TrsfTarget_Server}
	if runnerCID != "" {
		cid, err := objproto.ParseConnectionID(runnerCID, objproto.ParseOption_ResolveAddr)
		if err != nil {
			return nil, fmt.Errorf("trsf: parse runner cid %q: %w", runnerCID, err)
		}
		body.Target = protocol.TrsfTarget_Runner
		body.RunnerCid = protocol.ConnIDToRunnerID(cid)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_TrsfState}
	req.SetTrsfState(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_TrsfState {
		return nil, fmt.Errorf("trsf: unexpected response kind %v", resp.Kind)
	}
	r := resp.TrsfState()
	if r == nil {
		return nil, errors.New("trsf: response variant missing")
	}
	switch r.Status {
	case protocol.TrsfStateStatus_Ok:
		// The rows come on a stream, as ConnListWith's do: a TaskControl
		// response is one application message that has to fit a path MTU, and
		// ten of these rows already exceed udp's 1200.
		if r.StreamId == 0 {
			return nil, fmt.Errorf("trsf: server returned no stream id")
		}
		st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
		if st == nil {
			return nil, fmt.Errorf("trsf: stream %d not visible after the response", r.StreamId)
		}
		var raw []byte
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			data, eof, rerr := st.ReadDirect(64 * 1024)
			if rerr != nil {
				return nil, fmt.Errorf("trsf: stream read: %w", rerr)
			}
			raw = append(raw, data...)
			if eof {
				break
			}
		}
		body := &protocol.TrsfStateResultBody{}
		if derr := body.DecodeExact(raw); derr != nil {
			return nil, fmt.Errorf("trsf: decode body (%d bytes): %w", len(raw), derr)
		}
		return body.Conns, nil
	case protocol.TrsfStateStatus_RunnerOffline:
		return nil, ErrTrsfRunnerOffline
	case protocol.TrsfStateStatus_NotPermitted:
		return nil, ErrTrsfNotPermitted
	default:
		return nil, ErrTrsfUnavailable
	}
}
