package server

import (
	"context"
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleListPortForwards streams the visible port-forward snapshot over a
// server-initiated trsf send-stream. Mirrors handleListConns: the
// TaskControlResponse{ListPortForwards} carries only the stream id, and the
// actual PortForwardListResultBody is encoded onto the stream so the response
// fits in any path MTU.
func (h *TaskHandler) handleListPortForwards(conn ConnHandle, requestID uint32, connID string, filter protocol.TaskID, askEndpoints bool) {
	if askEndpoints && h.ForwardEndpointDropsFn != nil {
		// OFF the receive goroutine, and not as an optimisation. Filling the
		// endpoint drops asks the forward's own CLIENT, which is very often the
		// caller issuing this listing -- and a connection's inbound messages
		// are handled serially, so the answer would arrive on the goroutine
		// blocked waiting for it. The same deadlock handleTrsfState's client
		// branch has a comment about.
		go h.listPortForwards(conn, requestID, connID, filter, true)
		return
	}
	h.listPortForwards(conn, requestID, connID, filter, false)
}

func (h *TaskHandler) listPortForwards(conn ConnHandle, requestID uint32, connID string, filter protocol.TaskID, askEndpoints bool) {
	respond := func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ListPortForwards, RequestId: requestID}
		resp.SetListPortForwards(protocol.PortForwardListResult{StreamId: streamID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	forwards := h.visiblePortForwards(connID, filter)
	if askEndpoints && h.ForwardEndpointDropsFn != nil {
		h.ForwardEndpointDropsFn(context.Background(), forwards)
	}

	var body protocol.PortForwardListResultBody
	body.SetForwards(forwards)

	bodyBytes, err := body.EncodeCopy(nil)
	if err != nil {
		slog.Error("ListPortForwards: encode body failed", "err", err)
		respond(0)
		return
	}

	stream := conn.CreateSendStream()
	if stream == nil {
		respond(0)
		return
	}
	if werr := stream.AppendData(false, bodyBytes); werr != nil {
		slog.Warn("ListPortForwards: stream write failed", "err", werr)
		_ = stream.Close()
		respond(0)
		return
	}
	if werr := stream.AppendData(true); werr != nil {
		slog.Warn("ListPortForwards: stream EOF failed", "err", werr)
		_ = stream.Close()
		respond(0)
		return
	}
	respond(uint64(stream.ID()))
}

// visiblePortForwards returns the registrations connID may see, reaping any whose
// task has left Running/Detached on the way past. Visibility mirrors
// handleListConns: an operator (zero principal) or a caller whose visibility
// rank is global sees
// everything; anyone else sees only forwards for tasks in its own subtree.
//
// The reap is deliberately lazy — here, at the single call site — rather than
// hooked into TaskStore.Finish/Cancel/MarkFailed and the runner-loss paths.
// Intercepting a shared transition at some of its call sites is a failure mode
// this repo has already paid for (see implementation-pitfalls Pitfall 3 / 10).
func (h *TaskHandler) visiblePortForwards(connID string, filter protocol.TaskID) []protocol.PortForwardInfo {
	all, allowed := h.visibleToCaller(connID)
	filterHex := ""
	if filter.Id != ([16]byte{}) {
		filterHex = hex.EncodeToString(filter.Id[:])
	}
	var out []protocol.PortForwardInfo
	for _, pf := range h.pforwards().list() {
		task, ok := h.Tasks.Get(pf.taskIDHex)
		if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
			h.closePortForward(pf, protocol.PortForwardCloseReason_TaskGone, "")
			continue
		}
		if !all && !allowed[pf.taskIDHex] {
			continue
		}
		if filterHex != "" && pf.taskIDHex != filterHex {
			continue
		}
		out = append(out, portForwardInfo(pf))
	}
	return out
}

// portForwardInfo converts a registration into its wire form.
func portForwardInfo(pf *portForward) protocol.PortForwardInfo {
	var info protocol.PortForwardInfo
	info.ForwardId = pf.forwardID
	info.Direction = pf.direction
	if b, err := hex.DecodeString(pf.taskIDHex); err == nil && len(b) == 16 {
		copy(info.TaskId.Id[:], b)
	}
	info.SetBindAddr([]byte(pf.bindAddr))
	info.BindPort = pf.bindPort
	info.SetTargetHost([]byte(pf.targetHost))
	info.TargetPort = pf.targetPort
	info.OriginKind = pf.clientKind
	info.SetOriginCid([]byte(pf.clientCID))
	info.ClientEndpoint = pf.clientEndpoint
	info.Protocol = pf.protocolKind
	info.Route = pf.route
	info.BytesToTarget, info.BytesFromTarget, info.ConnsTotal, info.ConnsOpen,
		info.LastActivityUnixMs = pf.counters()
	info.Taps = pf.tapCount()
	if pf.link != nil {
		info.SetLink(*pf.link) // Protocol, set above, is ip: the arm exists
	}
	// The datagram group, as a group, zeros included -- and ONLY on a row that
	// carries datagrams (udp, ip). That
	// split is the existence condition the counter list exists to express: a tcp
	// forward has no max datagram size and cannot drop a datagram, while a udp
	// row that has dropped nothing must still say so. "0 packets overflowed" is
	// the answer to "why will QUIC not connect", and an absent key there would
	// read as "this row does not report it".
	if pf.protocolKind.CarriesDatagrams() {
		info.SetCounter(protocol.ForwardCounterKey_MaxDatagramSize, uint64(pf.maxDatagramSize.Load()))
		// What THIS SERVER's relay dropped, and nothing else. An endpoint's own
		// drops are not here and cannot be: a datagram the client refused never
		// reached this process. Those arrive per hop, under their own keys, and
		// only when a caller asked -- see fillEndpointDrops.
		info.SetForwardDropCounters(protocol.ForwardHopRelay, protocol.ForwardDropsBody{
			DroppedOversize:   pf.droppedOversize.Load(),
			DroppedCongestion: pf.droppedCongestion.Load(),
			DroppedQueue:      pf.droppedQueue.Load(),
		})
	}
	return info
}

// closePortForward drops a registration, tells the client why, and releases the
// runner listener for remote forwards. Reports whether THIS call removed it:
// registry.remove is the single point of arbitration, so two concurrent killers
// cannot both be told they succeeded.
func (h *TaskHandler) closePortForward(pf *portForward, reason protocol.PortForwardCloseReason, detail string) bool {
	return h.teardownPortForward(pf, reason, detail, true)
}

// teardownPortForward is the single tail shared by every path that ends a
// registration: drop it from the registry, and when the runner holds state for
// it (runnerHoldsState) tell the runner to release that state, so no orphan
// listener, udp registration or tun survives. When notify is
// true, a `closed` event is pushed on the control stream first — for callers
// whose client did not already learn about the teardown some other way (a
// kill, or the lazy task-gone reap). watchRemoteForwardControl passes
// notify=false: it only runs after the client's own control stream already
// EOF'd, so the client already knows it hung up and reason is unused.
// Reports whether THIS call removed it: registry.remove is the single point
// of arbitration, so two concurrent teardowns cannot both be told they
// succeeded.
func (h *TaskHandler) teardownPortForward(pf *portForward, reason protocol.PortForwardCloseReason, detail string, notify bool) bool {
	if _, ok := h.pforwards().remove(pf.forwardID); !ok {
		return false
	}
	if notify {
		pushPortForwardClosed(pf, reason, detail)
	}
	// Taps are told unconditionally, even when notify is false. notify governs
	// the OWNER's control stream, and a tapper is not necessarily the owner: it
	// has no other channel on which to learn the difference between "the
	// forward ended" and "my own connection dropped".
	pf.closeTaps(reason, detail)
	// The single funnel every ending path goes through, so a subscriber's row
	// disappears whether it was killed, reaped for a gone task, or dropped with
	// its client's connection.
	h.emitForwardEvent(protocol.StatusEventKind_ForwardClosed, pf)
	if !pf.runnerHoldsState() {
		return true
	}
	// By identity: on a runner_gone teardown this reaches the runner's NEW
	// connection, whose session holds nothing for the id -- a no-op there.
	runner, ok := h.Registry.GetByIdentity(pf.runnerID)
	if !ok || runner.Conn == nil {
		return true
	}
	sendClosePortForward(runner.Conn, pf.forwardID)
	return true
}

// DropPortForwardsForConn deregisters every forward registered over connID, and
// releases the runner listener behind each Remote one. Called from the server's
// connection teardown.
//
// This is not redundant with watchRemoteForwardControl. That watcher fires when
// the PEER CLOSES the control stream, which only a client that ran its own
// cleanup does — `RunForward` returning normally, or Ctrl-C. A client that dies
// abruptly never closes anything: SIGKILL, or the SIGHUP a terminal sends its
// foreground group when its window is closed. The watcher's ReadDirect stays
// parked on a stream whose transport is gone, so without this hook the
// registration outlives the connection indefinitely — verified live before the
// fix: a SIGKILLed `harness-cli forward -L` was still listed 60s later, its
// listener already released, its connection already gone from activeConns.
//
// That is the headline case for this whole feature: "I lost track of which
// terminal held it, so I closed the window" leaves exactly this ghost.
//
// notify=false: there is no one left to tell, and the control stream is dead.
func (h *TaskHandler) DropPortForwardsForConn(connID string) {
	for _, pf := range h.pforwards().list() {
		if pf.clientCID != connID {
			continue
		}
		h.teardownPortForward(pf, protocol.PortForwardCloseReason_Killed, "", false)
	}
}

// runnerHoldsState reports whether the runner keeps state for this forward that
// only ClosePortForward (or the runner's connection ending) releases: an -R
// listener, a udp registration, a link's tun. A tcp -L holds none -- each
// connection is opened afresh -- which is why it alone survives a runner
// reconnect.
func (pf *portForward) runnerHoldsState() bool {
	return pf.direction == protocol.PortForwardDirection_Remote ||
		pf.direction == protocol.PortForwardDirection_Link ||
		pf.protocolKind == protocol.ForwardProtocol_Udp
}

// DropRunnerStateForwards ends every forward whose runner-side state lived on
// runnerCID, because that state is gone: RunHandle.Close released it when the
// connection ended, and a runner on a new connection starts with a fresh
// session. Before this the row stayed listed and silent -- the relay, which
// resolves the runner by identity, sent the client's datagrams to the new
// connection, which dropped them uncounted.
//
// notify=true: the CLIENT is alive and must learn its forward ended, and why.
// Called from a runner connection's teardown and from a takeover (Registry.Add
// displacing the old connection), whose teardown would otherwise arrive only
// at the server's ping timeout.
func (h *TaskHandler) DropRunnerStateForwards(runnerCID string) {
	if runnerCID == "" {
		return
	}
	for _, pf := range h.pforwards().list() {
		if pf.runnerCID != runnerCID {
			continue
		}
		h.teardownPortForward(pf, protocol.PortForwardCloseReason_RunnerGone, "", true)
	}
}

// handleForwardEnded tears a forward down when its runner end reports that it
// stopped working after setup (a tun read or write failed). Only the
// connection holding the forward may say so: a forward_id is an identifier,
// not an authorisation, the same rule the datagram relay applies.
func (h *TaskHandler) handleForwardEnded(conn ConnHandle, msg *protocol.ForwardEnded) {
	pf, ok := h.pforwards().get(msg.ForwardId)
	if !ok || conn == nil || pf.runnerCID == "" || pf.runnerCID != conn.ConnectionID().String() {
		return
	}
	h.teardownPortForward(pf, protocol.PortForwardCloseReason_EndpointFailed, "runner: "+string(msg.Detail), true)
}

// killPortForward closes one registration on request. An id the caller cannot see
// answers no_such_forward, identical to an unknown id: a distinct "denied" would
// let a confined agent probe ids to learn which forwards exist.
func (h *TaskHandler) killPortForward(connID string, id uint64) protocol.KillPortForwardStatus {
	pf, ok := h.pforwards().get(id)
	if !ok {
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	all, allowed := h.visibleToCaller(connID)
	if !all && !allowed[pf.taskIDHex] {
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	if !h.closePortForward(pf, protocol.PortForwardCloseReason_Killed, "") {
		// Someone else removed it between the get and the remove.
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	return protocol.KillPortForwardStatus_Ok
}
