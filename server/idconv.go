package server

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// boardRunnerIDFromProto converts a wire protocol.RunnerID (as carried in
// ClientHello.AgentInfo) to the agentboard.RunnerID the Registry/Board key on.
// Field-for-field copy; structurally identical but distinct Go types (agentboard
// does not import protocol).
func boardRunnerIDFromProto(p protocol.RunnerID) agentboard.RunnerID {
	var out agentboard.RunnerID
	out.SetTransport(p.Transport)
	// Guard: the protocol encoder asserts ip_addr_len ∈ {4,16}; substitute
	// IPv4 placeholder when the wire value is absent or malformed.
	ip := p.IpAddr
	if len(ip) != 4 && len(ip) != 16 {
		ip = []byte{0, 0, 0, 0}
	}
	out.SetIpAddr(ip)
	out.Port = p.Port
	out.UniqueNumber = p.UniqueNumber
	return out
}

// boardTaskIDFromProto converts a wire protocol.TaskID to the agentboard.TaskID
// used as a Registry/Board key. Field-for-field copy of the fixed-size Id array.
func boardTaskIDFromProto(p protocol.TaskID) agentboard.TaskID {
	var out agentboard.TaskID
	out.Id = p.Id
	return out
}
