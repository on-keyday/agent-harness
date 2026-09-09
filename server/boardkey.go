package server

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The agentboard keys a task's auth ticket AND its taskState (subscriptions,
// attached conns, sender attestation) by the pair (runner identity, task id) —
// agentboard/registry.go, agentboard/board.go.
//
// The runner half used to be derived from the runner's ConnectionID, which made
// the pair a CONNECTION-lifetime key: the same runner process reconnecting
// produced a different key, so a surviving agent's ticket stopped validating.
// The original design wrote that premise down and accepted it, because a task
// could not outlive its runner's connection anyway
// (docs/superpowers/specs/2026-04-28-agent-comms-design.md §5.1). It is now a
// RunnerID proper — the identity of a runner PROCESS — so the key lives as long
// as the task does.
//
// These wrappers exist so no call site composes that key. Every server-side
// register / revoke / lookup names the RUNNER and the TASK, and this file is the
// only place that decides what the board is keyed by.
//
// All three are nil-Board safe: the handlers are constructed with Board unset in
// test wiring (task_handler.go's Board == nil degradation), and every caller
// used to carry its own `if X.Board != nil` guard for exactly that reason.

// boardRegisterTask stores the task's fresh ticket and seeds its inbound topic.
//
// A zero identity is refused rather than registered. The identity gate already
// rejects a hello that carries one, so reaching here with zero means an entry
// was built by some path that bypassed the gate; registering anyway would put
// the ticket under a key every such runner shares, and RegisterTask overwrites.
// Failing loudly beats handing out a credential that another dispatch will
// silently invalidate.
func boardRegisterTask(b *agentboard.Board, runner protocol.RunnerID, taskIDHex string, ticket [16]byte, agentProfile string) {
	if b == nil {
		return
	}
	if runner.IsZero() {
		slog.Error("board: refusing to register a task under the zero runner identity",
			"task", taskIDHex)
		return
	}
	b.RegisterTask(runner, taskIDFromHex(taskIDHex), ticket, agentProfile)
}

// boardRevokeTask drops the ticket and destroys the taskState, so an agent of a
// finished (or rolled-back) task can no longer authenticate.
func boardRevokeTask(b *agentboard.Board, runner protocol.RunnerID, taskIDHex string) {
	if b == nil {
		return
	}
	b.Revoke(runner, taskIDFromHex(taskIDHex))
}

// boardTaskTicket returns the ticket already registered for the task. Looked
// up, never reissued: Register overwrites, so minting a fresh one here would
// invalidate the credential a running agent is holding
// (agentboard/registry.go, registry.Ticket). Absent ticket yields the zero
// value and false.
//
// The task id arrives typed rather than hex because its caller holds it that
// way — an exec request carries protocol.TaskID off the wire.
func boardTaskTicket(b *agentboard.Board, runner protocol.RunnerID, tid protocol.TaskID) ([16]byte, bool) {
	if b == nil {
		return [16]byte{}, false
	}
	return b.Registry().Ticket(runner, tid)
}

// identityOfConn resolves a runner CONNECTION id to the identity the board keys
// by. Exactly one caller needs it — TaskFinished arrives carrying the
// connection it came in on and nothing else — so it lives here rather than as a
// Registry method: every other call site already holds the RunnerEntry and
// names entry.Identity directly.
//
// A miss yields the zero identity rather than an error. That is deliberate: the
// entry is gone, so there is nothing left to revoke, and a zero key matches
// nothing in the board.
func identityOfConn(reg *Registry, runnerConnID string) protocol.RunnerID {
	if reg == nil {
		return protocol.RunnerID{}
	}
	e, ok := reg.Get(runnerConnID)
	if !ok {
		return protocol.RunnerID{}
	}
	return e.Identity
}
