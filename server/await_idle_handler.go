package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// defaultAwaitIdleThreshold is the server default for AwaitIdleRequest.
// ThresholdMs == 0. Basis (measured 2026-07-12 against claude): an in-flight
// agent TUI repaints its spinner ~every 100ms with a max observed inter-read
// gap of ~0.5s, while an idle prompt emits nothing at all — 2500ms is 5× the
// busy-side max gap.
const defaultAwaitIdleThreshold = 2500 * time.Millisecond

// handleAwaitIdle arms a one-shot idle watcher on a live interactive
// session's SessionMux.
//
// It requires NO capability. It used to require exec_attach, on the reasoning
// that it is read-only observation of a session — true, but the fact it
// observes is last_output_at, which `ls` already returns to any caller that can
// see the task at all. Gating only the edge-triggered path made the efficient
// route cost strictly more authority than polling for the same answer, which
// protects nothing and taxes the cheap path. The scope check below is the real
// gate: a session outside the caller's reach reports NotFound. The lifecycle
// and program legs observe what `ls` shows too (lifecycle, program_state).
//
// sink=notify is the one exception and it is gated below, on `notify` — the
// egress, not the observation, is what needs authority.
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
	requestID := req.Header.RequestId
	respond := func(status protocol.AwaitIdleStatus, f IdleFire, watcherID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: requestID}
		lo := uint64(0)
		if f.LastOutput > 0 {
			lo = uint64(f.LastOutput)
		}
		resp.SetAwaitIdle(protocol.AwaitIdleResponse{
			Status: status, LastOutputAt: lo, WatcherId: watcherID,
			FiredBy: f.FiredBy, Lifecycle: f.Lifecycle, ProgramState: f.ProgramState, ProgramKind: f.ProgramKind,
		})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	// Target gate. This is the ONLY gate for sink=reply/board; there is no
	// capability half any more (see the doc comment), so this is the
	// scope half, and an out-of-scope session is reported as absent.
	// Resolved through the VISIBILITY set, not an action capability. The doc
	// comment above is the reason: what this observes is last_output_at, which
	// `ls` already returns to any caller that can see the task. Resolving it
	// through exec_view would reintroduce exactly what dropping the capability
	// removed — the edge-triggered route costing more authority than polling
	// for the same answer, since a narrowed exec_view would refuse here while
	// `ls` kept answering.
	if !h.inScope(conn.ConnectionID().String(), protocol.Capability_None, hex.EncodeToString(ai.TaskId.Id[:])) {
		respond(protocol.AwaitIdleStatus_NotFound, IdleFire{}, 0)
		return
	}

	topic := string(ai.Topic)
	switch ai.Sink {
	case protocol.AwaitSink_Reply:
	case protocol.AwaitSink_Notify:
		// The notify sink reaches the operator-notification egress
		// (notify-hook → e.g. the operator's phone), which the Notify RPC
		// gates behind Capability_Notify. Require the SAME cap here — this is
		// now the ONLY gate on await-idle, and without it a confined task
		// could spam operator notifications through await-idle that it could
		// not send via `harness-cli notify`. (The fire text is
		// server-synthesized, so this is a noise vector, not spoofing — but
		// an egress gate that depends on which RPC you arrive by is not a
		// gate.) The board sink is a publish, so it takes the requester's
		// board reach instead (below), as agent send does.
		if !hasCap(h.callerCaps(conn.ConnectionID().String()), protocol.Capability_Notify) {
			h.denyTaskControl(conn, protocol.TaskControlKind_AwaitIdle, requestID, protocol.Capability_Notify)
			return
		}
	case protocol.AwaitSink_Board:
		if topic == "" || h.Board == nil {
			respond(protocol.AwaitIdleStatus_BadRequest, IdleFire{}, 0)
			return
		}
		// The fire publishes a row stamped with the requester and wakes the
		// topic's subscribers: a task may arm it only on a topic it could
		// publish to itself (reach spec §2). An operator connection has no
		// principal task and is not reach-checked, like the rest of the
		// operator face.
		if h.lookupPrincipal(conn.ConnectionID().String()).Id != ([16]byte{}) &&
			!h.Board.Publishable(h.boardState(conn), topic) {
			h.denyTaskControl(conn, protocol.TaskControlKind_AwaitIdle, requestID, protocol.Capability_BoardPeer)
			return
		}
	default:
		respond(protocol.AwaitIdleStatus_BadRequest, IdleFire{}, 0)
		return
	}
	if !validAwaitIdleSignal(ai.Signal) {
		respond(protocol.AwaitIdleStatus_BadRequest, IdleFire{}, 0)
		return
	}

	taskIDHex := hex.EncodeToString(ai.TaskId.Id[:])
	mux := h.sessionMux(taskIDHex)
	if mux == nil {
		respond(protocol.AwaitIdleStatus_NotFound, IdleFire{}, 0)
		return
	}

	threshold := time.Duration(ai.ThresholdMs) * time.Millisecond
	if threshold == 0 {
		threshold = defaultAwaitIdleThreshold
	}
	// Every signal reports the lifecycle reading (spec D8), so the reader is
	// set whatever leg the watcher waits on. No refusal for a task that never
	// reported: the watcher waits (spec D10).
	spec := IdleWatchSpec{Signal: ai.Signal, Threshold: threshold, Lifecycle: func() LifecycleReading {
		t, ok := h.Tasks.Get(taskIDHex)
		if !ok {
			return LifecycleReading{}
		}
		return LifecycleReading{State: t.Lifecycle, At: t.LifecycleAt, WorkingAt: t.LifecycleWorkingAt}
	}}

	// Resolve the requester's identity NOW — by fire time the conn may be
	// gone (sink=notify/board deliberately outlive the request).
	requesterConnID := conn.ConnectionID().String()
	requester := h.lookupPrincipal(requesterConnID)

	w := &idleWatcher{
		taskIDHex:  taskIDHex,
		sink:       ai.Sink,
		signal:     ai.Signal,
		topic:      topic,
		threshold:  threshold,
		requester:  requester,
		clientCID:  requesterConnID,
		clientKind: h.lookupClientKind(requesterConnID),
		stop:       make(chan struct{}),
	}
	// Set before add publishes w: a kill from another connection may read it
	// the moment the entry is in the registry. w.id is read at call time,
	// which is after add assigned it.
	if ai.Sink == protocol.AwaitSink_Reply {
		w.onCancel = func() { respond(protocol.AwaitIdleStatus_Cancelled, mux.idleReadings(spec), w.id) }
	}
	// Registered BEFORE arming: an already-idle session fires on the first
	// check, and the fire must find its own entry to remove.
	id := h.idleWatchers().add(w)

	var deliver func(f IdleFire)
	switch ai.Sink {
	case protocol.AwaitSink_Reply:
		deliver = func(f IdleFire) {
			st := protocol.AwaitIdleStatus_Fired
			if f.Stopped {
				st = protocol.AwaitIdleStatus_SessionStopped
			}
			respond(st, f, id)
		}
	case protocol.AwaitSink_Notify:
		respond(protocol.AwaitIdleStatus_Armed, mux.idleReadings(spec), id)
		deliver = func(f IdleFire) {
			h.fireIdleNotify(taskIDHex, requesterConnID, f)
		}
	case protocol.AwaitSink_Board:
		respond(protocol.AwaitIdleStatus_Armed, mux.idleReadings(spec), id)
		deliver = func(f IdleFire) {
			h.fireIdleBoard(topic, taskIDHex, requester, ai.Signal, f)
		}
	}
	mux.ArmIdleWatcher(spec, w.stop, func(f IdleFire) {
		// A kill or a teardown that removed it first owns the outcome.
		if _, still := h.idleWatchers().remove(id); !still {
			return
		}
		deliver(f)
	})
}

// idleNotifyText is the notification line for a fire: the quiet leg keeps
// its "since last output" wording, the state legs name what they saw.
func idleNotifyText(taskIDHex string, f IdleFire) string {
	short := taskIDHex
	if len(short) > 8 {
		short = short[:8]
	}
	switch {
	case f.Stopped:
		return fmt.Sprintf("session %s ended", short)
	case f.FiredBy != protocol.AwaitIdleSignal_Quiet:
		return fmt.Sprintf("session %s idle (%s)", short,
			cli.AwaitIdleFiredDetail(f.FiredBy, f.Lifecycle, f.ProgramState, f.ProgramKind))
	default:
		idle := time.Since(time.Unix(0, f.LastOutput))
		return fmt.Sprintf("session %s idle (%ds since last output)", short, int(idle/time.Second))
	}
}

// fireIdleNotify delivers a fired idle watcher through the notify path: the
// live leg (OnNotify → ring + topic) and the egress leg (NotifyHook), same
// two legs as a client-sent notify. Origin=external: the text was
// synthesized by the server, not sent from inside a worker.
func (h *TaskHandler) fireIdleNotify(taskIDHex, requesterConnID string, f IdleFire) {
	text := idleNotifyText(taskIDHex, f)
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

// sessionIdlePayload is the board fire's JSON. kind/task/last_output_at_unix_ms/
// status are the original keys; the rest report the signal and the readings
// (await-idle signal spec D8), wire words with "" for unset/none.
type sessionIdlePayload struct {
	Kind         string `json:"kind"`
	Task         string `json:"task"`
	LastOutputMs int64  `json:"last_output_at_unix_ms"`
	Status       string `json:"status"`
	Signal       string `json:"signal"`
	FiredBy      string `json:"fired_by"`
	Lifecycle    string `json:"lifecycle"`
	ProgramState string `json:"program_state"`
	ProgramKind  string `json:"program_kind"`
}

// fireIdleBoard publishes a fired idle watcher to the agentboard topic the
// requester named (typically its own chat.<short-id> inbound channel, so the
// arming agent's inbox hook wakes it). Attributed to the requester's
// principal task (zero for an operator) with a zero RunnerID, and stamped
// sender_kind=server.
func (h *TaskHandler) fireIdleBoard(topic, taskIDHex string, requester protocol.TaskID, signal protocol.AwaitIdleSignal, f IdleFire) {
	status := "fired"
	if f.Stopped {
		status = "session_stopped"
	}
	payload, err := json.Marshal(sessionIdlePayload{
		Kind:         "session_idle",
		Task:         taskIDHex,
		LastOutputMs: f.LastOutput / int64(time.Millisecond),
		Status:       status,
		Signal:       cli.AwaitIdleSignalString(signal),
		FiredBy:      cli.AwaitIdleSignalString(f.FiredBy),
		Lifecycle:    cli.LifecycleWord(f.Lifecycle),
		ProgramState: cli.ProgramWord(f.ProgramState),
		ProgramKind:  cli.ProgramKindWord(f.ProgramKind),
	})
	if err != nil {
		slog.Warn("await-idle: encode board payload", "err", err)
		return
	}
	// The agent profile is empty because this publish originates in the
	// server itself, not in any agent. The kind is what identifies it; the
	// "server" hostname stays for readers that predate the kind.
	if _, _, err := h.Board.Send(topic, payload, protocol.RunnerID{}, requester, "server", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Server)); err != nil {
		slog.Warn("await-idle: board publish failed", "topic", topic, "task", taskIDHex, "err", err)
	}
}
