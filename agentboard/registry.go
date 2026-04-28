package agentboard

import (
	"crypto/subtle"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Aliases for shorter usage at call sites; map to brgen-generated HelloStatus_*.
const (
	HelloStatusOk             = HelloStatus_Ok
	HelloStatusBadTicket      = HelloStatus_BadTicket
	HelloStatusUnknownTask    = HelloStatus_UnknownTask
	HelloStatusRunnerMismatch = HelloStatus_RunnerMismatch
)

type ticketKey struct {
	runner string
	task   string
}

type registry struct {
	mu      sync.Mutex
	tickets map[ticketKey][16]byte
}

func newRegistry() *registry {
	return &registry{tickets: make(map[ticketKey][16]byte)}
}

// Register stores a ticket keyed by the (protocol.RunnerID, protocol.TaskID) pair.
// Server-side TryDispatch and OpenInteractive use this when issuing a fresh ticket.
func (r *registry) Register(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickets[ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}] = ticket
}

// Revoke removes a ticket entry. Idempotent: revoking an unknown key is a no-op.
func (r *registry) Revoke(rid protocol.RunnerID, tid protocol.TaskID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tickets, ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)})
}

// Validate is called from the agent_message Hello handler with the
// agentboard.RunnerID/TaskID types decoded off the wire.
func (r *registry) Validate(rid RunnerID, tid TaskID, ticket [16]byte) HelloStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	want, ok := r.tickets[ticketKey{runner: runnerIDStringBoard(rid), task: hexTaskIDBoard(tid)}]
	if !ok {
		return HelloStatusUnknownTask
	}
	if subtle.ConstantTimeCompare(want[:], ticket[:]) != 1 {
		return HelloStatusBadTicket
	}
	return HelloStatusOk
}
