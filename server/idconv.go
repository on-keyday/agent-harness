package server

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// boardRunnerIDFromProto converts a wire protocol.RunnerID (as carried in
// ClientHello.AgentInfo) to the agentboard.RunnerID the Registry/Board key on:
// the same 16 opaque bytes under a distinct Go type, because agentboard does
// not import protocol.
//
// It used to copy four address fields and substitute an IPv4 placeholder for an
// absent value, because the board's schema refused ip_addr_len == 0 and the
// encoder asserted on it. Both are gone; a zero identity copies as zero, which
// is what an unidentified sender should be.
func boardRunnerIDFromProto(p protocol.RunnerID) agentboard.RunnerID {
	var out agentboard.RunnerID
	out.Id = p.Id
	return out
}

// boardTaskIDFromProto converts a wire protocol.TaskID to the agentboard.TaskID
// used as a Registry/Board key. Field-for-field copy of the fixed-size Id array.
func boardTaskIDFromProto(p protocol.TaskID) agentboard.TaskID {
	var out agentboard.TaskID
	out.Id = p.Id
	return out
}
