package agentboard

import (
	"crypto/subtle"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Aliases for shorter usage at call sites.
//
// They name protocol.ClientHelloStatus values now. The board used to declare a
// HelloStatus enum of its own, carrying the same four meanings, with a
// conversion function on the server between them -- one of several types that
// existed only because a second .bgn file cannot reference the first one's.
const (
	HelloStatusOk             = protocol.ClientHelloStatus_Ok
	HelloStatusBadTicket      = protocol.ClientHelloStatus_BadTicket
	HelloStatusUnknownTask    = protocol.ClientHelloStatus_UnknownTask
	HelloStatusRunnerMismatch = protocol.ClientHelloStatus_RunnerMismatch
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

// Ticket returns the ticket registered for (rid, tid), if any.
//
// Reuse, not reissue: a second Register for the same pair OVERWRITES the entry,
// which would invalidate the credential the running agent is already holding.
// A caller that needs to hand the task's identity to another process it starts
// in that task's name — `exec` does — must look the existing one up.
func (r *registry) Ticket(rid protocol.RunnerID, tid protocol.TaskID) ([16]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tickets[ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}]
	return t, ok
}

// Validate is called from the ClientHello path with the identity the agent
// presented.
func (r *registry) Validate(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) protocol.ClientHelloStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	want, ok := r.tickets[ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}]
	if !ok {
		return HelloStatusUnknownTask
	}
	if subtle.ConstantTimeCompare(want[:], ticket[:]) != 1 {
		return HelloStatusBadTicket
	}
	return HelloStatusOk
}
