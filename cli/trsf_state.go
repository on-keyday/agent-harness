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
// target says whose: the SERVER's own connections, a RUNNER's, or a CLIENT's.
// The last is the only way to see a client's transport at all -- it is neither
// of the other two, and a datagram its trsf dropped inside the run loop never
// reached the client's own application code as an error either.
//
// peer.CID names the peer for the two non-server targets, and the answer says
// which host it came from by the role on each row.
//
// The second return is when the ANSWERER sampled, by its own clock. Every
// counter on a row is read as a rate, and the interval has to be measured where
// the counters advanced: timing it here divides one host's delta by another's
// elapsed, which is how a share-of-the-interval column came to print 135%.
// Only the difference of two of these is ever used, and both come from the same
// host, so no clock is compared against another's.
func (c *Client) TrsfStateOn(ctx context.Context, peer TrsfPeer) ([]protocol.TrsfConnState, int64, error) {
	body := protocol.TrsfStateRequest{Target: peer.Target}
	if peer.Target != protocol.TrsfTarget_Server {
		if peer.CID == "" {
			return nil, 0, fmt.Errorf("trsf: target %v needs a connection id", peer.Target)
		}
		cid, err := objproto.ParseConnectionID(peer.CID, objproto.ParseOption_ResolveAddr)
		if err != nil {
			return nil, 0, fmt.Errorf("trsf: parse %v cid %q: %w", peer.Target, peer.CID, err)
		}
		body.PeerCid = protocol.ConnIDFromObjproto(cid)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_TrsfState}
	req.SetTrsfState(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if resp.Kind != protocol.TaskControlKind_TrsfState {
		return nil, 0, fmt.Errorf("trsf: unexpected response kind %v", resp.Kind)
	}
	r := resp.TrsfState()
	if r == nil {
		return nil, 0, errors.New("trsf: response variant missing")
	}
	switch r.Status {
	case protocol.TrsfStateStatus_Ok:
		// The rows come on a stream, as ConnListWith's do: a TaskControl
		// response is one application message that has to fit a path MTU, and
		// ten of these rows already exceed udp's 1200.
		if r.StreamId == 0 {
			return nil, 0, fmt.Errorf("trsf: server returned no stream id")
		}
		st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
		if st == nil {
			return nil, 0, fmt.Errorf("trsf: stream %d not visible after the response", r.StreamId)
		}
		var raw []byte
		for {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			data, eof, rerr := st.ReadDirect(64 * 1024)
			if rerr != nil {
				return nil, 0, fmt.Errorf("trsf: stream read: %w", rerr)
			}
			raw = append(raw, data...)
			if eof {
				break
			}
		}
		body := &protocol.TrsfStateResultBody{}
		if derr := body.DecodeExact(raw); derr != nil {
			return nil, 0, fmt.Errorf("trsf: decode body (%d bytes): %w", len(raw), derr)
		}
		return body.Conns, int64(body.SampledUnixNs), nil
	case protocol.TrsfStateStatus_RunnerOffline:
		return nil, 0, ErrTrsfRunnerOffline
	case protocol.TrsfStateStatus_NotPermitted:
		return nil, 0, ErrTrsfNotPermitted
	default:
		return nil, 0, ErrTrsfUnavailable
	}
}

// TrsfPeer names whose transport state to read: the server's own connections,
// or a named runner's or client's.
//
// One value rather than the two mutually exclusive strings the flags arrive as.
// Threading those side by side through every surface would put the "exactly
// one of these" rule wherever someone remembered it, and make a signature that
// takes both read as though both could be set.
type TrsfPeer struct {
	Target protocol.TrsfTarget
	CID    string
}

// TrsfPeerFor resolves the two peer flags, once, at the edge that reads them.
func TrsfPeerFor(runnerCID, clientCID string) (TrsfPeer, error) {
	switch {
	case runnerCID != "" && clientCID != "":
		return TrsfPeer{}, errors.New("trsf: --runner and --client name different peers; pass one")
	case runnerCID != "":
		return TrsfPeer{Target: protocol.TrsfTarget_Runner, CID: runnerCID}, nil
	case clientCID != "":
		return TrsfPeer{Target: protocol.TrsfTarget_Client, CID: clientCID}, nil
	default:
		return TrsfPeer{Target: protocol.TrsfTarget_Server}, nil
	}
}
