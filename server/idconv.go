package server

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// boardRunnerIDFromProto converts a wire protocol.RunnerID (as carried in
// ClientHello.AgentInfo) to the agentboard.RunnerID the Registry/Board key on.
// Field-for-field copy; structurally identical but distinct Go types (agentboard
// does not import protocol). Mirrors the former client-side protoToBoardRunnerID.
func boardRunnerIDFromProto(p protocol.RunnerID) agentboard.RunnerID {
	var out agentboard.RunnerID
	out.SetTransport(p.Transport)
	out.SetIpAddr(p.IpAddr)
	out.Port = p.Port
	out.UniqueNumber = p.UniqueNumber
	return out
}

func boardTaskIDFromProto(p protocol.TaskID) agentboard.TaskID {
	var out agentboard.TaskID
	out.Id = p.Id
	return out
}
