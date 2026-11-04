package server

import (
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runner kick — gated on runner_enroll by requiredCap
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md §2).
// The runner is asked to leave and exits; the server keeps no ban list (D8).

const defaultKickWait = 10 * time.Second

func (h *TaskHandler) handleRunnerKick(conn ConnHandle, requestID uint32, r *protocol.RunnerKickRequest) {
	reply := func(status protocol.RunnerKickStatus, body *protocol.RunnerKickBody) {
		send := func(streamID uint64) {
			resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerKick, RequestId: requestID}
			resp.SetRunnerKick(protocol.RunnerKickResponse{RequestId: requestID, Status: status, StreamId: streamID})
			respondAgent(conn, resp)
		}
		if body == nil {
			send(0)
			return
		}
		respondWithBody(conn, "runner_kick", body, send)
	}

	prefix := strings.ToLower(string(r.Prefix))
	var matches []RunnerEntry
	if len(prefix) >= 8 && len(prefix) <= 32 {
		for _, e := range h.Registry.List() {
			if strings.HasPrefix(e.Identity.Hex(), prefix) {
				matches = append(matches, e)
			}
		}
	}
	switch len(matches) {
	case 0:
		reply(protocol.RunnerKickStatus_NotFound, nil)
		return
	case 1:
	default:
		reply(protocol.RunnerKickStatus_AmbiguousPrefix, nil)
		return
	}
	e := matches[0]

	body := &protocol.RunnerKickBody{}
	user := protocol.RunnerKeyUser{Id: e.Identity}
	user.SetHostname([]byte(e.Hostname))
	body.Runner = user
	body.SetTasks(activeTaskIDs(e))

	// D14: a runner with live tasks needs --force, and the answer names them.
	if len(body.Tasks) > 0 && !r.Force() {
		reply(protocol.RunnerKickStatus_InUse, body)
		return
	}

	// D9: every record that would bring the runner back goes first, so no
	// re-dial races the leave.
	var removed []uint32
	if h.Dials != nil && h.RunnerDials != nil {
		for _, id := range h.Dials.RecordsPointingAt(e) {
			if _, err := h.RunnerDials.Remove(id); err == nil {
				removed = append(removed, id)
			}
			h.Dials.Stop(id)
		}
	}
	body.SetRemovedDials(removed)

	by := h.enrolledBy(conn.ConnectionID().String())
	leave := protocol.LeaveRequest{}
	leave.SetBy([]byte(by))
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_Leave}
	req.SetLeave(leave)
	if err := sendRunnerRequest(e.Conn, req); err != nil {
		slog.Warn("runner kick: send leave failed", "runner", e.Identity.Hex(), "err", err)
	}
	slog.Warn("runner kicked", "runner", e.Identity.Hex(), "host", e.Hostname, "by", by,
		"tasks", len(body.Tasks), "removed_dials", len(removed))

	// The wait runs off the dispatch goroutine: a runner too old to know
	// `leave` (D15) would otherwise hold this connection's other requests for
	// the whole window.
	wait := h.KickWait
	if wait == 0 {
		wait = defaultKickWait
	}
	go func() {
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			if _, ok := h.Registry.Get(e.ID); !ok {
				reply(protocol.RunnerKickStatus_Left, body)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if h.KickConn != nil {
			h.KickConn(e.ID)
		}
		reply(protocol.RunnerKickStatus_ClosedWithoutLeaving, body)
	}()
}

func activeTaskIDs(e RunnerEntry) []protocol.TaskID {
	hexes := make([]string, 0, len(e.ActiveTasks))
	for id := range e.ActiveTasks {
		hexes = append(hexes, id)
	}
	sort.Strings(hexes)
	out := make([]protocol.TaskID, 0, len(hexes))
	for _, s := range hexes {
		var t protocol.TaskID
		if b, err := hex.DecodeString(s); err == nil && len(b) == len(t.Id) {
			copy(t.Id[:], b)
			out = append(out, t)
		}
	}
	return out
}
