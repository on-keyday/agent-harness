package server

import (
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
)

// authorizeJoin decides one pubsub JOIN for a connection that authenticated as
// role (spec 2026-10-27-pubsub-authorization).
//
// The role is why this is asked per connection. A runner connection has no
// principal task, and every visibility helper reads "no principal" as an
// operator, so asking them about a runner would hand it every topic. A runner
// may JOIN only the log of a task it owns, and is the only connection whose
// writes are published.
func (s *Server) authorizeJoin(role protocol.AuthRole, cid objproto.ConnectionID, topic string) pubsub.JoinGrant {
	taskHex, isLog := topics.ParseTaskLog(topic)
	switch role {
	case protocol.AuthRole_Runner:
		if isLog && s.registry.ownsTask(cid, taskHex) {
			return pubsub.JoinGrant{Allow: true, Publish: true}
		}
	case protocol.AuthRole_Client:
		switch {
		case isLog:
			// The `logs` gate. Refused alike for no exec_view, out of scope and
			// no such task, so the refusal is not an existence oracle.
			return pubsub.JoinGrant{Allow: s.taskHandler.authorize(cid.String(), protocol.Capability_ExecView, taskHex)}
		case topic == topics.Notifications():
			// No snapshot verb to mirror, so refused rather than left silent.
			return pubsub.JoinGrant{Allow: s.seesEverything(cid)}
		}
		// Everything else filters per publish (runners.status included: `watch`
		// joins it, and a confined `watch` gets no runner rows, as its `ls` does).
		return pubsub.JoinGrant{Allow: true}
	}
	return pubsub.JoinGrant{}
}

// seesEverything is `ls`'s test for showing runners: an operator, or a caller
// whose visibility rank is global.
func (s *Server) seesEverything(cid objproto.ConnectionID) bool {
	all, _ := s.taskHandler.visibleToCaller(cid.String())
	return all
}

// taskLogDelivery is the per-publish filter for what the owning runner writes
// to a task log: the JOIN gate again, so a `caps set` that drops exec_view
// stops the next chunk. The runner itself passes as an operator would — only
// its own task's log admitted it.
func (s *Server) taskLogDelivery(topic string) func(objproto.ConnectionID) bool {
	taskHex, ok := topics.ParseTaskLog(topic)
	if !ok {
		return nil
	}
	return func(cid objproto.ConnectionID) bool {
		return s.taskHandler.authorize(cid.String(), protocol.Capability_ExecView, taskHex)
	}
}
