package server

import (
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// ConnHandle is the minimal interface a handler needs to identify, reply to,
// and stream bulk data back to a peer. Decoupled from the concrete
// objproto.Connection / trsf.Transport so tests can fake it.
//
// CreateSendStream returns a server-initiated unidirectional stream. Handlers
// use it for responses too large to fit in a single objproto message
// (e.g. GetTaskLog returning a full log file). May return nil in tests where
// the fake doesn't wire trsf.
type ConnHandle interface {
	ConnectionID() objproto.ConnectionID
	SendMessage(b []byte) (int, uint64, error)
	CreateSendStream() trsf.SendStream
}

type Dispatcher struct {
	OnRunnerControl func(ConnHandle, []byte) // payload is everything after the kind byte
	OnTaskControl   func(ConnHandle, []byte)
}

// Dispatch routes msg by inspecting the first byte (the wire kind).
// If msg is empty, Dispatch is a no-op.
// Unknown / unhandled kinds are ignored silently.
func (d *Dispatcher) Dispatch(conn ConnHandle, msg []byte) {
	if len(msg) == 0 {
		return
	}

	kind := wire.ApplicationPayloadKind(msg[0])
	payload := msg[1:]

	switch kind {
	case wire.ApplicationPayloadKind_RunnerControl:
		if d.OnRunnerControl != nil {
			d.OnRunnerControl(conn, payload)
		}
	case wire.ApplicationPayloadKind_TaskControl:
		if d.OnTaskControl != nil {
			d.OnTaskControl(conn, payload)
		}
	}
}
