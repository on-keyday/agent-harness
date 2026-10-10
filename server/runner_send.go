package server

import (
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// runnerSender is the part of a runner connection a send needs. ConnHandle
// satisfies it.
type runnerSender interface {
	ConnectionID() objproto.ConnectionID
	SendMessage([]byte) (int, uint64, error)
	MaxDatagramSize() int
}

// sendRunnerRequest encodes req and sends it to a runner — refusing one that
// would not fit one datagram on a udp connection, which would otherwise be
// dropped with no error at either end. Every RunnerRequest goes out here
// (TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest), so the check cannot
// be skipped by a new call site.
func sendRunnerRequest(conn runnerSender, req *protocol.RunnerRequest) error {
	data, err := req.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return err
	}
	if err := protocol.CheckControlMessage(conn.ConnectionID().Transport, conn.MaxDatagramSize(), len(data)); err != nil {
		return err
	}
	_, _, err = conn.SendMessage(data)
	return err
}
