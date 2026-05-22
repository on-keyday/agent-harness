package runner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// proxyHandlerState bundles the inputs the agent-proxy ceremony needs from
// the runner: the current server-conn CID (for collision detection and the
// SetProxy allocate-side construction), whether the server conn is live,
// and a task lookup. Extracted as a struct so the validation logic is
// pure-function-testable without spinning up real endpoints.
type proxyHandlerState struct {
	serverCID     objproto.ConnectionID
	hasServerConn bool
	taskExists    func(taskID protocol.TaskID) bool
}

// validateProxyRequest computes the response status for a given agent CID
// and requested task ID, applying the four error conditions in the spec.
func (s *proxyHandlerState) validateProxyRequest(agentCID objproto.ConnectionID, taskID protocol.TaskID) protocol.ProxyEstablishResponse {
	if !s.hasServerConn {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_ServerNotConnected}
	}
	if agentCID.ID == s.serverCID.ID {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_IdCollision}
	}
	if !s.taskExists(taskID) {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_UnknownTask}
	}
	return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_Ok}
}

// allocateCID constructs the SetProxy "allocate" CID. It uses the server's
// transport + addr (so the forward goes via the existing server-conn entry
// in transport.connMap) with the agent's connection_id (so the server sees
// a new conn rather than collision with the runner-server conn).
func (s *proxyHandlerState) allocateCID(agentCID objproto.ConnectionID) objproto.ConnectionID {
	return objproto.NewConnectionID(s.serverCID.Transport, s.serverCID.Addr, agentCID.ID)
}

// runAgentProxyCeremony validates the request, calls SetProxy on the endpoint
// when Ok, and sends the EstablishResponse back to the agent on pc. Caller is
// responsible for pc.Close() AFTER this returns (per spec ordering constraint:
// SetProxy → ack → Close).
func runAgentProxyCeremony(
	ctx context.Context,
	logger *slog.Logger,
	st *proxyHandlerState,
	ep objproto.Endpoint,
	pc *peer.Conn,
	req protocol.ProxyRequest,
) error {
	_ = ctx // reserved for future cancellation hooks; current impl is synchronous
	agentCID := pc.Connection().ConnectionID()
	resp := st.validateProxyRequest(agentCID, req.TaskId)

	if resp.Status == protocol.ProxyEstablishStatus_Ok {
		alloc := st.allocateCID(agentCID)
		if err := ep.SetProxy(agentCID, alloc); err != nil {
			if logger != nil {
				logger.Error("agent proxy: SetProxy failed",
					"agent_cid", agentCID.String(),
					"alloc_cid", alloc.String(),
					"err", err)
			}
			return fmt.Errorf("SetProxy: %w", err)
		}
		if logger != nil {
			logger.Info("agent proxy: established",
				"agent_cid", agentCID.String(),
				"alloc_cid", alloc.String(),
				"task_id", fmt.Sprintf("%x", req.TaskId.Id))
		}
	}

	// Send response on the active peer.Conn. Order: SetProxy (above) →
	// SendMessage (here) → caller's pc.Close().
	var envelope protocol.ProxyControl
	envelope.Kind = protocol.ProxyControlKind_EstablishResponse
	envelope.SetEstablishResponse(resp)
	payload := envelope.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentProxyControl)})
	if _, _, err := pc.Connection().SendMessage(payload); err != nil {
		return fmt.Errorf("send EstablishResponse: %w", err)
	}
	return nil
}
