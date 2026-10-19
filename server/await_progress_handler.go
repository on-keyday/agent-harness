package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleAwaitProgress arms a one-shot watcher on a task's next progress write.
//
// Gated exactly as handleAwaitIdle: the target through the visibility set (what
// it reveals, the progress line, `ls` already hands to any caller that can see
// the task), sink=notify on `notify`, sink=board on the requester's board
// reach. Unlike await-idle it needs no live session, so a oneshot task can be
// watched.
//
// since_progress_at is the baseline: a stored progress_at newer than it fires
// at once, so a write landing between the caller's `ls` and its arm is not
// missed. That check runs before the terminal check, so a line written just
// before the task ended is delivered rather than task_ended.
func (h *TaskHandler) handleAwaitProgress(conn ConnHandle, req *protocol.TaskControlRequest) {
	ap := req.AwaitProgress()
	requestID := req.RequestId
	respond := func(status protocol.AwaitProgressStatus, at int64, text string, watcherID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AwaitProgress, RequestId: requestID}
		r := protocol.AwaitProgressResponse{Status: status, ProgressAt: uint64(at), WatcherId: watcherID}
		r.SetProgress([]byte(text))
		resp.SetAwaitProgress(r)
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	if ap == nil {
		respond(protocol.AwaitProgressStatus_BadRequest, 0, "", 0)
		return
	}
	connID := conn.ConnectionID().String()
	taskIDHex := hex.EncodeToString(ap.TaskId.Id[:])
	if _, ok := h.Tasks.Get(taskIDHex); !ok || !h.inScope(connID, protocol.Capability_None, taskIDHex) {
		respond(protocol.AwaitProgressStatus_NotFound, 0, "", 0)
		return
	}

	topic := string(ap.Topic)
	switch ap.Sink {
	case protocol.AwaitSink_Reply:
	case protocol.AwaitSink_Notify:
		// The same egress gate as await-idle's notify sink, for the same
		// reason: without it a confined task could reach the operator's
		// notification egress by a route `harness-cli notify` refuses it.
		if !hasCap(h.callerCaps(connID), protocol.Capability_Notify) {
			h.denyTaskControl(conn, protocol.TaskControlKind_AwaitProgress, requestID, protocol.Capability_Notify)
			return
		}
	case protocol.AwaitSink_Board:
		if topic == "" || h.Board == nil {
			respond(protocol.AwaitProgressStatus_BadRequest, 0, "", 0)
			return
		}
		if h.lookupPrincipal(connID).Id != ([16]byte{}) && !h.Board.Publishable(h.boardState(conn), topic) {
			h.denyTaskControl(conn, protocol.TaskControlKind_AwaitProgress, requestID, protocol.Capability_BoardPeer)
			return
		}
	default:
		respond(protocol.AwaitProgressStatus_BadRequest, 0, "", 0)
		return
	}

	requester := h.lookupPrincipal(connID)
	w := &idleWatcher{
		taskIDHex:  taskIDHex,
		condition:  protocol.AwaitCondition_Progress,
		sink:       ap.Sink,
		topic:      topic,
		requester:  requester,
		clientCID:  connID,
		clientKind: h.lookupClientKind(connID),
		// Nothing runs on it — there is no mux goroutine — but kill and
		// teardown close it, so it must exist.
		stop: make(chan struct{}),
	}
	switch ap.Sink {
	case protocol.AwaitSink_Reply:
		w.fireProgress = func(st protocol.AwaitProgressStatus, text string, at int64) { respond(st, at, text, w.id) }
		w.onCancel = func() { respond(protocol.AwaitProgressStatus_Cancelled, 0, "", w.id) }
	case protocol.AwaitSink_Notify:
		w.fireProgress = func(st protocol.AwaitProgressStatus, text string, at int64) {
			h.fireProgressNotify(taskIDHex, connID, st, text)
		}
	case protocol.AwaitSink_Board:
		w.fireProgress = func(st protocol.AwaitProgressStatus, text string, at int64) {
			h.fireProgressBoard(topic, taskIDHex, requester, st, text, at)
		}
	}
	// Registered BEFORE reading the store: a write racing this arm either
	// lands after registration and fires the watcher through
	// fireProgressWatchers, or before it and is seen by the baseline check
	// below. remove() decides which path delivers.
	id := h.idleWatchers().add(w)

	// notify/board answer armed now and deliver later; reply's answer IS
	// the delivery.
	if ap.Sink != protocol.AwaitSink_Reply {
		respond(protocol.AwaitProgressStatus_Armed, 0, "", id)
	}
	t, ok := h.Tasks.Get(taskIDHex)
	switch {
	case !ok:
		h.endProgressWatcher(id, protocol.AwaitProgressStatus_TaskEnded, "", 0)
	case ap.SinceProgressAt != 0 && t.ProgressAt > int64(ap.SinceProgressAt):
		h.endProgressWatcher(id, protocol.AwaitProgressStatus_Fired, t.Progress, t.ProgressAt)
	case t.Status == protocol.TaskStatus_Succeeded || t.Status == protocol.TaskStatus_Failed || t.Status == protocol.TaskStatus_Cancelled:
		h.endProgressWatcher(id, protocol.AwaitProgressStatus_TaskEnded, "", 0)
	}
}

// endProgressWatcher delivers one watcher's outcome if this call is the one
// that removes it; a fire, kill or teardown that got there first owns it.
func (h *TaskHandler) endProgressWatcher(id uint64, status protocol.AwaitProgressStatus, text string, at int64) {
	w, still := h.idleWatchers().remove(id)
	if !still {
		return
	}
	close(w.stop)
	w.fireProgress(status, text, at)
}

// fireProgressWatchers ends every progress-condition watcher on taskHex with
// the given outcome: Fired after a write, TaskEnded when the task ends or is
// pruned. Idle watchers on the same task are untouched.
func (h *TaskHandler) fireProgressWatchers(taskHex string, status protocol.AwaitProgressStatus, text string, at int64) {
	for _, w := range h.idleWatchers().list(taskHex) {
		if w.condition != protocol.AwaitCondition_Progress {
			continue
		}
		h.endProgressWatcher(w.id, status, text, at)
	}
}

// fireProgressNotify delivers through the notify path, both legs, as
// fireIdleNotify does. Origin=external: the server wrote this text around the
// task's line, the task did not send it.
func (h *TaskHandler) fireProgressNotify(taskIDHex, requesterConnID string, status protocol.AwaitProgressStatus, text string) {
	short := taskIDHex
	if len(short) > 8 {
		short = short[:8]
	}
	body := fmt.Sprintf("progress %s: %s", short, text)
	if status == protocol.AwaitProgressStatus_TaskEnded {
		body = fmt.Sprintf("task %s ended", short)
	}
	ts := time.Now().Unix()
	ev := protocol.NotifyEvent{Ts: uint64(ts), Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_External}
	ev.SetTitle([]byte("await-progress"))
	ev.SetText([]byte(body))
	if h.OnNotify != nil {
		h.OnNotify(ev)
	}
	runNotifyHook(h.NotifyHook, notifyHookPayload{
		Level:  protocol.NotifyLevel_Info.String(),
		Origin: protocol.NotifyOrigin_External.String(),
		Title:  "await-progress",
		Text:   body,
		ConnID: requesterConnID,
		Ts:     ts,
		TaskID: taskIDHex,
	})
}

// fireProgressBoard publishes the outcome to the topic the requester named,
// attributed to the requester's principal and stamped sender_kind=server, as
// fireIdleBoard does. The line is JSON-encoded, never spliced into the text.
func (h *TaskHandler) fireProgressBoard(topic, taskIDHex string, requester protocol.TaskID, status protocol.AwaitProgressStatus, text string, at int64) {
	st := "fired"
	if status == protocol.AwaitProgressStatus_TaskEnded {
		st = "task_ended"
	}
	payload, err := json.Marshal(struct {
		Kind       string `json:"kind"`
		Task       string `json:"task"`
		Progress   string `json:"progress"`
		ProgressAt int64  `json:"progress_at_unix_ms"`
		Status     string `json:"status"`
	}{"task_progress", taskIDHex, text, at / int64(time.Millisecond), st})
	if err != nil {
		slog.Warn("await-progress: encode board payload failed", "task", taskIDHex, "err", err)
		return
	}
	if _, _, err := h.Board.Send(topic, payload, protocol.RunnerID{}, requester, "server", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Server)); err != nil {
		slog.Warn("await-progress: board publish failed", "topic", topic, "task", taskIDHex, "err", err)
	}
}
