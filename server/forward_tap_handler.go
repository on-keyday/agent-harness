package server

import (
	"context"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// streamTapSink writes tap records onto the client's stream.
//
// Records are CONCATENATED, with no length prefix of their own: every
// ForwardTapRecord is self-delimiting under its own schema, so the reader
// decodes one and keeps the remainder. A length prefix would be a wire byte the
// schema does not describe, which is the one thing this project's message
// format is not allowed to have.
type streamTapSink struct {
	stream trsf.BidirectionalStream
}

func (s *streamTapSink) send(rec *protocol.ForwardTapRecord) error {
	buf, err := rec.EncodeCopy(nil)
	if err != nil {
		return err
	}
	return s.stream.AppendData(false, buf)
}

// forwardVisibleTo reports whether connID may see pf at all. Factored out of
// visiblePortForwards so the listing and the tap decide visibility with one
// predicate rather than two that can drift apart.
func (h *TaskHandler) forwardVisibleTo(connID string, pf *portForward) bool {
	all, allowed := h.visibleToCaller(connID)
	return all || allowed[pf.taskIDHex]
}

// handleOpenForwardTap attaches a tap to one forward and answers with the
// stream its records arrive on.
//
// Three gates, and they are not interchangeable:
//
//  1. Holding forward_tap at all — checked before dispatch, off requiredCap,
//     answered with a PermissionDenied response rather than a status here.
//  2. Visibility — an id the caller cannot see answers no_such_forward, the
//     same answer an unknown id gets. Forward ids come from a dense next++
//     counter, so a distinguishable "exists, but not yours" would be an
//     enumeration oracle.
//  3. Scope — a forward visible through a global visibility rank may still
//     belong to a task outside the caller's ACTION scope, which is the same
//     distinction kill_port_forward draws. inScope rather than authorize: the
//     hasCap half authorize would repeat has already run in (1).
//
// Visibility before scope, for the reason in (2): denying on scope for a
// forward the caller cannot see would leak the same fact.
func (h *TaskHandler) handleOpenForwardTap(conn ConnHandle, req *protocol.OpenForwardTapRequest, connID string) protocol.OpenForwardTapResponse {
	errResp := func(s protocol.OpenForwardTapStatus) protocol.OpenForwardTapResponse {
		return protocol.OpenForwardTapResponse{Status: s}
	}
	pf, ok := h.pforwards().get(req.ForwardId)
	if !ok || !h.forwardVisibleTo(connID, pf) {
		return errResp(protocol.OpenForwardTapStatus_NoSuchForward)
	}
	if !h.inScope(connID, protocol.Capability_ForwardTap, pf.taskIDHex) {
		return errResp(protocol.OpenForwardTapStatus_NoSuchForward)
	}
	if conn == nil {
		slog.Error("forward tap: nil client conn (programmer error)")
		return errResp(protocol.OpenForwardTapStatus_InternalError)
	}
	stream := conn.CreateBidirectionalStream()
	if stream == nil {
		return errResp(protocol.OpenForwardTapStatus_InternalError)
	}

	tap := newForwardTap(&streamTapSink{stream: stream}, req.DirectionFilter, req.MaxRecordBytes)
	pf.addTap(tap)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		defer pf.removeTap(tap)
		defer func() { _ = stream.CloseBoth() }()
		tap.run(ctx)
	}()
	// Watch for the tapper going away. The client never writes on this stream,
	// so any read returning EOF or error means it is gone — the same signal
	// watchRemoteForwardControl reads for a forward's control stream.
	//
	// Without this a tap is only reaped when the NEXT record fails to send, so
	// a tap closed on a quiet forward is never noticed: it stays attached, and
	// `taps=N` keeps counting a reader that left. Observed exactly that way —
	// the TUI's tap view closed and the row still said taps=1.
	go func() {
		defer cancel()
		for {
			_, eof, err := stream.ReadDirect(4096)
			if eof || err != nil {
				return
			}
		}
	}()

	slog.Info("forward tap: attached", "forward_id", pf.forwardID, "task_id", pf.taskIDHex,
		"filter", req.DirectionFilter, "max_record_bytes", req.MaxRecordBytes)
	return protocol.OpenForwardTapResponse{
		Status:   protocol.OpenForwardTapStatus_Ok,
		StreamId: uint64(stream.ID()),
	}
}
