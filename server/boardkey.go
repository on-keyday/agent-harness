package server

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The agentboard keys a task's auth ticket AND its taskState (subscriptions,
// attached conns, sender attestation) by the pair (runner identity, task id) —
// agentboard/registry.go, agentboard/board.go. The runner half is derived from
// the runner's ConnectionID, so the pair is a CONNECTION-lifetime key, not a
// task-lifetime one: reconnecting the same runner process yields a different
// key, because runnerIDFromConnID carries Port and UniqueNumber.
//
// That is sound only while a task cannot outlive its runner's connection. The
// original design wrote that premise down explicitly — "runner_id は ephemeral
// (再接続で変わる) だが、agent ≈ task の寿命と一致するため identity 切れは発生
// しない" (docs/superpowers/specs/2026-04-28-agent-comms-design.md §5.1) — and
// failAndRevokeTasksOf is what still enforces it.
//
// These wrappers exist so no call site composes that key. Every server-side
// register / revoke / lookup names the RUNNER CONNECTION and the TASK, and this
// file is the only place that decides what the board is keyed by. Anything that
// changes the premise — a task held across a reconnect, a runner identity
// separated from its ConnectionID — changes this file rather than a dozen
// handlers, and the compiler finds every caller because they pass a connection
// id, not a RunnerID.
//
// All three are nil-Board safe: the handlers are constructed with Board unset in
// test wiring (task_handler.go's Board == nil degradation), and every caller
// used to carry its own `if X.Board != nil` guard for exactly that reason.

// boardRegisterTask stores the task's fresh ticket and seeds its inbound topic.
func boardRegisterTask(b *agentboard.Board, runnerConnID, taskIDHex string, ticket [16]byte, agentProfile string) {
	if b == nil {
		return
	}
	b.RegisterTask(runnerIDFromConnID(runnerConnID), taskIDFromHex(taskIDHex), ticket, agentProfile)
}

// boardRevokeTask drops the ticket and destroys the taskState, so an agent of a
// finished (or rolled-back) task can no longer authenticate.
func boardRevokeTask(b *agentboard.Board, runnerConnID, taskIDHex string) {
	if b == nil {
		return
	}
	b.Revoke(runnerIDFromConnID(runnerConnID), taskIDFromHex(taskIDHex))
}

// boardTaskTicket returns the ticket already registered for the task. Looked
// up, never reissued: Register overwrites, so minting a fresh one here would
// invalidate the credential a running agent is holding
// (agentboard/registry.go, registry.Ticket). Absent ticket yields the zero
// value and false.
//
// The task id arrives typed rather than hex because its caller holds it that
// way — an exec request carries protocol.TaskID off the wire.
func boardTaskTicket(b *agentboard.Board, runnerConnID string, tid protocol.TaskID) ([16]byte, bool) {
	if b == nil {
		return [16]byte{}, false
	}
	return b.Registry().Ticket(runnerIDFromConnID(runnerConnID), tid)
}
