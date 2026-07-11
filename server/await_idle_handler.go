package server

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// defaultAwaitIdleThreshold is the server default for AwaitIdleRequest.
// ThresholdMs == 0. Basis (measured 2026-07-12 against claude): an in-flight
// agent TUI repaints its spinner ~every 100ms with a max observed inter-read
// gap of ~0.5s, while an idle prompt emits nothing at all — 2500ms is 5× the
// busy-side max gap.
const defaultAwaitIdleThreshold = 2500 * time.Millisecond

// handleAwaitIdle arms a one-shot idle watcher on a live interactive
// session's SessionMux. Capability gating (exec_attach, same as
// AttachSession — this is read-only observation of a session) already
// happened in Handle via the requiredCap map.
//
// sink=reply long-polls: the response is deferred until the watcher fires
// (request_id correlation makes a delayed TaskControlResponse safe). If the
// requester's conn is gone by then, the send is a harmless error — the
// watcher is one-shot and gone either way, no leak.
func (h *TaskHandler) handleAwaitIdle(conn ConnHandle, req *protocol.TaskControlRequest) {
	ai := req.AwaitIdle()
	if ai == nil {
		slog.Error("TaskHandler: AwaitIdle variant is nil")
		return
	}
	requestID := req.RequestId
	respond := func(status protocol.AwaitIdleStatus, lastOutputUnixNano int64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: requestID}
		lo := uint64(0)
		if lastOutputUnixNano > 0 {
			lo = uint64(lastOutputUnixNano)
		}
		resp.SetAwaitIdle(protocol.AwaitIdleResponse{Status: status, LastOutputAt: lo})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	topic := string(ai.Topic)
	switch ai.Sink {
	case protocol.AwaitIdleSink_Reply, protocol.AwaitIdleSink_Notify:
	case protocol.AwaitIdleSink_Board:
		if topic == "" || h.Board == nil {
			respond(protocol.AwaitIdleStatus_BadRequest, 0)
			return
		}
	default:
		respond(protocol.AwaitIdleStatus_BadRequest, 0)
		return
	}

	taskIDHex := hex.EncodeToString(ai.TaskId.Id[:])
	mux := h.sessionMux(taskIDHex)
	if mux == nil {
		respond(protocol.AwaitIdleStatus_NotFound, 0)
		return
	}

	threshold := time.Duration(ai.ThresholdMs) * time.Millisecond
	if threshold == 0 {
		threshold = defaultAwaitIdleThreshold
	}

	// Resolve the requester's identity NOW — by fire time the conn may be
	// gone (sink=notify/board deliberately outlive the request).
	requesterConnID := conn.ConnectionID().String()
	requester := h.lookupPrincipal(requesterConnID)

	switch ai.Sink {
	case protocol.AwaitIdleSink_Reply:
		mux.ArmIdleWatcher(threshold, func(stopped bool, lo int64) {
			st := protocol.AwaitIdleStatus_Fired
			if stopped {
				st = protocol.AwaitIdleStatus_SessionStopped
			}
			respond(st, lo)
		})
	case protocol.AwaitIdleSink_Notify:
		respond(protocol.AwaitIdleStatus_Armed, mux.LastOutputUnixNano())
		mux.ArmIdleWatcher(threshold, func(stopped bool, lo int64) {
			h.fireIdleNotify(taskIDHex, requesterConnID, stopped, lo)
		})
	case protocol.AwaitIdleSink_Board:
		respond(protocol.AwaitIdleStatus_Armed, mux.LastOutputUnixNano())
		mux.ArmIdleWatcher(threshold, func(stopped bool, lo int64) {
			h.fireIdleBoard(topic, taskIDHex, requester, stopped, lo)
		})
	}
}

// fireIdleNotify delivers a fired idle watcher through the notify path: the
// live leg (OnNotify → ring + topic) and the egress leg (NotifyHook), same
// two legs as a client-sent notify. Origin=external: the text was
// synthesized by the server, not sent from inside a worker.
func (h *TaskHandler) fireIdleNotify(taskIDHex, requesterConnID string, stopped bool, lastOutputUnixNano int64) {
	short := taskIDHex
	if len(short) > 8 {
		short = short[:8]
	}
	var text string
	if stopped {
		text = fmt.Sprintf("session %s ended", short)
	} else {
		idle := time.Since(time.Unix(0, lastOutputUnixNano))
		text = fmt.Sprintf("session %s idle (%ds since last output)", short, int(idle/time.Second))
	}
	ts := time.Now().Unix()
	ev := protocol.NotifyEvent{
		Ts:     uint64(ts),
		Level:  protocol.NotifyLevel_Info,
		Origin: protocol.NotifyOrigin_External,
	}
	ev.SetTitle([]byte("await-idle"))
	ev.SetText([]byte(text))
	if h.OnNotify != nil {
		h.OnNotify(ev)
	}
	runNotifyHook(h.NotifyHook, notifyHookPayload{
		Level:  protocol.NotifyLevel_Info.String(),
		Origin: protocol.NotifyOrigin_External.String(),
		Title:  "await-idle",
		Text:   text,
		ConnID: requesterConnID,
		Ts:     ts,
		TaskID: taskIDHex,
	})
}

// fireIdleBoard publishes a fired idle watcher to the agentboard topic the
// requester named (typically its own chat.<short-id> inbound channel, so the
// arming agent's inbox hook wakes it). Attributed to the requester's
// principal task (zero for an operator) with a placeholder RunnerID — a
// zero-value RunnerID panics the encoder.
func (h *TaskHandler) fireIdleBoard(topic, taskIDHex string, requester protocol.TaskID, stopped bool, lastOutputUnixNano int64) {
	status := "fired"
	if stopped {
		status = "session_stopped"
	}
	payload := fmt.Sprintf(
		`{"kind":"session_idle","task":"%s","last_output_at_unix_ms":%d,"status":"%s"}`,
		taskIDHex, lastOutputUnixNano/int64(time.Millisecond), status)
	if _, err := h.Board.Send(topic, []byte(payload), placeholderRunnerID(), requester, "server"); err != nil {
		slog.Warn("await-idle: board publish failed", "topic", topic, "task", taskIDHex, "err", err)
	}
}
