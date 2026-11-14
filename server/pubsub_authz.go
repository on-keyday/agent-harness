package server

import (
	"strings"

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
// may JOIN nothing: the one topic it joined, its task's log, was retired in A3
// (A-D30). No connection is granted Publish, so nothing a subscriber writes on
// its stream reaches a topic; every topic's publisher is the server.
func (s *Server) authorizeJoin(role protocol.AuthRole, cid objproto.ConnectionID, topic string) pubsub.JoinGrant {
	if role != protocol.AuthRole_Client {
		return pubsub.JoinGrant{}
	}
	if topic == topics.Notifications() {
		// No snapshot verb to mirror, so refused rather than left silent.
		return pubsub.JoinGrant{Allow: s.seesEverything(cid)}
	}
	if strings.HasPrefix(topic, "task.") && strings.HasSuffix(topic, ".log") {
		// The retired task log (A3). Nothing publishes it; refusing the JOIN
		// ends an older `harness-cli logs -f` with an error instead of leaving
		// it waiting on a topic no one will write to.
		return pubsub.JoinGrant{}
	}
	// Everything else filters per publish (runners.status included: `watch`
	// joins it, and a confined `watch` gets no runner rows, as its `ls` does).
	return pubsub.JoinGrant{Allow: true}
}

// seesEverything is `ls`'s test for showing runners: an operator, or a caller
// whose visibility rank is global.
func (s *Server) seesEverything(cid objproto.ConnectionID) bool {
	all, _ := s.taskHandler.visibleToCaller(cid.String())
	return all
}
