package cli

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// clientTelemetry is what the server may ask this client about itself.
//
// Every other message on this connection goes client -> server. This direction
// exists because a client's own numbers are reachable from nowhere else:
// `conns --trsf` can target the server or a runner, and a client running a port
// forward is neither. It is not a convenience — a datagram dropped inside
// trsf's run loop, at the congestion gate after SendDatagram already returned
// nil, never reaches this process as an error, so no counter kept at the call
// site can see it. Asking the transport is the only way.
//
// An adapter rather than methods on Client, so answering stays one seam rather
// than two exported methods on the type every caller of this package holds.
type clientTelemetry struct{ c *Client }

// TrsfStates reports this client's one connection.
//
// Nothing here consults scope or capability. The server decides which rows a
// caller may see, because it is the only party that evaluates scope; a second
// implementation of that rule on the answering side is the drift the runner's
// own responder has a comment about.
func (t clientTelemetry) TrsfStates() []protocol.TrsfConnState {
	st := t.c.conn.Transport().GetInternalState()
	if st == nil {
		return nil
	}
	row := protocol.TrsfRowFrom(st)
	row.Role = protocol.ConnRole_Cli
	row.SetCid([]byte(t.c.conn.Connection().ConnectionID().String()))
	return []protocol.TrsfConnState{row}
}

// ForwardDrops answers from the process-wide forward registries, not from this
// Client: a forward id is assigned by the server and is unique across every
// Client in the process, which is why those registries are process-wide too.
func (t clientTelemetry) ForwardDrops(forwardID uint64) (protocol.ForwardDropsBody, bool) {
	return forwardDrops(forwardID)
}

// answerTelemetry is the one place this process ANSWERS rather than asks.
func (c *Client) answerTelemetry(payload []byte) {
	err := protocol.AnswerTelemetry(payload, clientTelemetry{c}, func(b []byte) error {
		_, _, serr := c.conn.Connection().SendMessage(b)
		return serr
	})
	if err != nil {
		slog.Warn("cli.Client: could not answer telemetry", "err", err)
	}
}
