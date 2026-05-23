package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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

// chainedRelayTimeout is how long runAgentProxyCeremony waits for the server
// to reply to RequestChainedRelay before rejecting the agent with InternalError.
const chainedRelayTimeout = 10 * time.Second

// runAgentProxyCeremony validates the request, asks the server to set up any
// required chained relay (RequestChainedRelay step), calls SetProxy on the
// local endpoint when the server confirms Ok or Direct, and sends the
// EstablishResponse back to the agent on pc. Caller is responsible for
// pc.Close() AFTER this returns (per spec ordering constraint:
// SetProxy → ack → Close).
//
// sess may be nil if the runner has no active server connection; in that case
// validateProxyRequest returns ServerNotConnected and the chained-relay step
// is skipped entirely.
func runAgentProxyCeremony(
	ctx context.Context,
	logger *slog.Logger,
	st *proxyHandlerState,
	ep objproto.Endpoint,
	pc *peer.Conn,
	req protocol.ProxyRequest,
	sess *Session,
) error {
	agentCID := pc.Connection().ConnectionID()
	resp := st.validateProxyRequest(agentCID, req.TaskId)

	if resp.Status == protocol.ProxyEstablishStatus_Ok {
		// Phase B + chained relay: before installing the local SetProxy, ask
		// the server to set up the upstream hops (if any). The server replies
		// with ChainedRelayStatus_Ok (chain ready) or ChainedRelayStatus_Direct
		// (runner is directly registered — no chain needed). Any other status
		// means the upstream setup failed and we must reject the agent.
		if sess != nil {
			slotID := agentCID.ID

			ch, err := sess.BeginChainedRelay()
			if err != nil {
				if logger != nil {
					logger.Warn("chained-relay: another in flight on this session", "err", err)
				}
				resp = protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
			} else {
				// Build and send RunnerMessage{RequestChainedRelay{slot_id}}.
				var rm protocol.RunnerMessage
				rm.Kind = protocol.RunnerMessageType_RequestChainedRelay
				rm.SetRequestChainedRelay(protocol.RequestChainedRelay{SlotId: slotID})
				payload := rm.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
				if err := sess.Sender.Send(payload); err != nil {
					if logger != nil {
						logger.Error("chained-relay: send RequestChainedRelay failed", "err", err)
					}
					// Clear the pending slot so a future ceremony can proceed.
					sess.DeliverChainedRelayResponse(protocol.ChainedRelayResponse{
						Status: protocol.ChainedRelayStatus_HopSetupFailed,
					})
					resp = protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
				} else {
					// Wait for the server's ChainedRelayResponse (or timeout/cancel).
					timer := time.NewTimer(chainedRelayTimeout)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						if logger != nil {
							logger.Warn("chained-relay: context cancelled waiting for response")
						}
						resp = protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
					case <-timer.C:
						if logger != nil {
							logger.Warn("chained-relay: timeout waiting for ChainedRelayResponse", "slot_id", slotID)
						}
						// Clear the pending slot so a future ceremony can proceed.
						sess.DeliverChainedRelayResponse(protocol.ChainedRelayResponse{
							Status: protocol.ChainedRelayStatus_HopSetupFailed,
						})
						resp = protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
					case cr := <-ch:
						switch cr.Status {
						case protocol.ChainedRelayStatus_Ok, protocol.ChainedRelayStatus_Direct:
							// Upstream chain is ready (or runner is direct). Proceed to local SetProxy.
							if logger != nil {
								logger.Info("chained-relay: server responded", "status", cr.Status, "slot_id", slotID)
							}
						default:
							if logger != nil {
								logger.Warn("chained-relay: server returned non-Ok status",
									"status", cr.Status, "slot_id", slotID)
							}
							resp = protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
						}
					}
				}
			}
		}
	}

	if resp.Status == protocol.ProxyEstablishStatus_Ok {
		alloc := st.allocateCID(agentCID)
		if err := ep.SetProxy(agentCID, alloc); err != nil {
			if logger != nil {
				logger.Error("agent proxy: SetProxy failed",
					"agent_cid", agentCID.String(),
					"alloc_cid", alloc.String(),
					"err", err)
			}
			// Convert the validation success → InternalError so the agent
			// gets an explicit signal (rather than just observing the conn
			// close). Fall through to the SendMessage path below.
			resp = protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_InternalError}
		} else {
			if logger != nil {
				logger.Info("agent proxy: established",
					"agent_cid", agentCID.String(),
					"alloc_cid", alloc.String(),
					"task_id", fmt.Sprintf("%x", req.TaskId.Id))
			}
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
