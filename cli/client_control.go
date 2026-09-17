package cli

import (
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// answerClientControl is the one place this process ANSWERS rather than asks.
//
// Every other message on this connection goes client -> server. This direction
// exists because a client's own transport state is reachable from nowhere else:
// `conns --trsf` can target the server or a runner, and a client running a port
// forward is neither. It is not a convenience — a datagram dropped inside
// trsf's run loop, at the congestion gate after SendDatagram already returned
// nil, never reaches this process as an error, so no counter kept at the call
// site can see it. Asking the transport is the only way.
//
// Nothing here consults scope or capability. The server decides which rows a
// caller may see, because it is the only party that evaluates scope; a second
// implementation of that rule living on the answering side is the drift the
// runner's own responder has a comment about.
func (c *Client) answerClientControl(payload []byte) {
	var req protocol.ClientControlRequest
	if _, err := req.Decode(payload); err != nil {
		slog.Error("cli.Client: decode ClientControlRequest", "err", err)
		return
	}
	switch req.Kind {
	case protocol.ClientControlKind_TrsfState:
		c.answerTrsfState(req.RequestId)
	default:
		// A kind this build does not know. Silent: the server asked something
		// newer than us, and a reply it cannot decode is worse than none.
		slog.Debug("cli.Client: unknown ClientControlKind", "kind", req.Kind)
	}
}

func (c *Client) answerTrsfState(requestID uint32) {
	st := c.conn.Transport().GetInternalState()
	if st == nil {
		slog.Warn("cli.Client: no transport state to answer with")
		return
	}
	row := protocol.TrsfRowFrom(st)
	row.Role = protocol.ConnRole_Cli
	row.SetCid([]byte(c.conn.Connection().ConnectionID().String()))

	var resp protocol.ClientControlResponse
	resp.Kind = protocol.ClientControlKind_TrsfState
	resp.RequestId = requestID
	resp.SetTrsfState(protocol.ClientTrsfStateBody{
		Count: 1,
		// Stamped HERE, by the clock the counters advanced against, and passed
		// through by the server rather than restamped: the delta between two
		// readings is the interval every counter on the row is read as a rate
		// over, and the server's clock is a round trip away.
		SampledUnixNs: uint64(time.Now().UnixNano()),
		Conns:         []protocol.TrsfConnState{row},
	})
	b, err := resp.Append([]byte{byte(appwire.AppKind_ClientControl)})
	if err != nil {
		slog.Error("cli.Client: encode ClientControlResponse", "err", err)
		return
	}
	if _, _, err := c.conn.Connection().SendMessage(b); err != nil {
		slog.Warn("cli.Client: could not answer trsf_state", "err", err)
	}
}
