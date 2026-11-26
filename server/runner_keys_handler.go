package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// The three runner key verbs (runner enroll / keys / unenroll). Each is gated
// on runner_enroll by authority.CentralCap before it reaches here, and then by
// that bit's SCOPE (bound enrollment, C1): an unbound key needs it global, a
// key bound to S needs every task in S inside it. Out of reach answers as an
// unknown key does, so a scoped caller cannot learn what lies outside.

// enrollReach is the caller's runner_enroll target set.
func (h *TaskHandler) enrollReach(cid objproto.ConnectionID) (all bool, allowed map[string]bool) {
	return h.scopeSet(cid, requiredCentral(protocol.TaskControlKind_RunnerEnroll))
}

// reachesBinding reports whether a runner_enroll target set covers a key
// bound to bound: all of it when unbound (an unbound runner takes any task,
// including ones not created yet), else every bound task.
func reachesBinding(all bool, allowed map[string]bool, bound []protocol.TaskID) bool {
	if all {
		return true
	}
	if len(bound) == 0 {
		return false
	}
	for _, id := range bound {
		if !allowed[hex.EncodeToString(id.Id[:])] {
			return false
		}
	}
	return true
}

// enrolledBy names who enrolled a key: the caller's principal task, or the
// operator for a connection that has none.
func (h *TaskHandler) enrolledBy(cid objproto.ConnectionID) string {
	p := h.lookupPrincipal(cid)
	if p == (protocol.TaskID{}) {
		return "operator"
	}
	return "task:" + hex.EncodeToString(p.Id[:])
}

func (h *TaskHandler) handleRunnerEnroll(conn ConnHandle, requestID uint32, r *protocol.RunnerEnrollRequest) {
	status := protocol.RunnerKeyStatus_Ok
	key := ed25519.PublicKey(append([]byte(nil), r.Key[:]...))
	switch {
	case h.RunnerKeys == nil || bytes.Equal(key, make([]byte, ed25519.PublicKeySize)):
		status = protocol.RunnerKeyStatus_InvalidKey
	default:
		bound, berr := normalizeBinding(r.Bound)
		if berr != nil {
			status = protocol.RunnerKeyStatus_BadBinding
			break
		}
		if all, allowed := h.enrollReach(conn.ConnectionID()); !reachesBinding(all, allowed, bound) {
			status = protocol.RunnerKeyStatus_OutOfReach
			break
		}
		added, err := h.RunnerKeys.Enroll(RunnerKeyRecord{
			Key: key, Label: string(r.Label), EnrolledAt: time.Now().UTC(),
			EnrolledBy: h.enrolledBy(conn.ConnectionID()), Bound: bound,
		})
		switch {
		case errors.Is(err, ErrBindingDiffers):
			status = protocol.RunnerKeyStatus_BindingDiffers
		case err != nil:
			slog.Error("runner enroll: store write failed", "err", err)
			status = protocol.RunnerKeyStatus_InvalidKey
		case !added:
			status = protocol.RunnerKeyStatus_AlreadyEnrolled
		default:
			slog.Info("runner key enrolled", "key", keyPrefix(key), "label", string(r.Label),
				"bound", len(bound), "by", h.enrolledBy(conn.ConnectionID()))
		}
	}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerEnroll, RequestId: requestID}
	resp.SetRunnerEnroll(protocol.RunnerEnrollResponse{Status: status})
	respondAgent(conn, resp)
}

// runnersOnKey lists the registered runners that authenticated with key.
func (h *TaskHandler) runnersOnKey(key ed25519.PublicKey) ([]protocol.RunnerKeyUser, []objproto.ConnectionID) {
	var users []protocol.RunnerKeyUser
	var conns []objproto.ConnectionID
	for _, e := range h.Registry.List() {
		if e.Key == nil || !e.Key.Equal(key) {
			continue
		}
		u := protocol.RunnerKeyUser{Id: e.Identity}
		u.SetHostname([]byte(e.Hostname))
		users = append(users, u)
		conns = append(conns, e.ID)
	}
	return users, conns
}

func (h *TaskHandler) handleRunnerUnenroll(conn ConnHandle, requestID uint32, r *protocol.RunnerUnenrollRequest) {
	reply := func(status protocol.RunnerKeyStatus, body *protocol.RunnerUnenrollBody) {
		send := func(streamID uint64) {
			resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerUnenroll, RequestId: requestID}
			resp.SetRunnerUnenroll(protocol.RunnerUnenrollResponse{RequestId: requestID, Status: status, StreamId: streamID})
			respondAgent(conn, resp)
		}
		if body == nil {
			send(0)
			return
		}
		respondWithBody(conn, "runner_unenroll", body, send)
	}

	prefix := strings.ToLower(string(r.Prefix))
	if len(prefix) < 8 || len(prefix) > 64 || h.RunnerKeys == nil {
		reply(protocol.RunnerKeyStatus_InvalidKey, nil)
		return
	}
	if _, err := hex.DecodeString(prefix[:len(prefix)&^1]); err != nil {
		reply(protocol.RunnerKeyStatus_InvalidKey, nil)
		return
	}
	all, allowed := h.enrollReach(conn.ConnectionID())
	rec, err := h.RunnerKeys.ResolveIn(prefix, func(r RunnerKeyRecord) bool { return reachesBinding(all, allowed, r.Bound) })
	switch {
	case errors.Is(err, ErrAmbiguousPrefix):
		reply(protocol.RunnerKeyStatus_AmbiguousPrefix, nil)
		return
	case err != nil:
		reply(protocol.RunnerKeyStatus_NotFound, nil)
		return
	}

	users, conns := h.runnersOnKey(rec.Key)
	body := &protocol.RunnerUnenrollBody{}
	copy(body.Key[:], rec.Key)
	body.SetLabel([]byte(rec.Label))
	body.SetRunners(users)
	body.SetBound(rec.Bound)
	// One key serves every slot of a machine by default, so removing one that
	// is in use cuts the whole machine off — including any agent on it that
	// would re-enroll. That takes --force.
	if len(users) > 0 && !r.Force() {
		reply(protocol.RunnerKeyStatus_InUse, body)
		return
	}
	if err := h.RunnerKeys.Remove(rec.Key); err != nil {
		reply(protocol.RunnerKeyStatus_NotFound, nil)
		return
	}
	// The key's dial records go with it (spec 2026-11-04 D5): a record bound
	// to a key nothing may register with would dial forever for nothing.
	body.SetRemovedDials(h.removeDialsOfKey(rec))
	slog.Warn("runner key unenrolled", "key", keyPrefix(rec.Key), "label", rec.Label,
		"by", h.enrolledBy(conn.ConnectionID()), "closing_runners", len(conns))
	if h.KickConn != nil {
		for _, c := range conns {
			h.KickConn(c)
		}
	}
	reply(protocol.RunnerKeyStatus_Ok, body)
}

func (h *TaskHandler) handleRunnerKeys(conn ConnHandle, requestID uint32) {
	body := &protocol.RunnerKeysBody{}
	var rows []protocol.RunnerKeyRow
	if h.RunnerKeys != nil {
		all, allowed := h.enrollReach(conn.ConnectionID())
		for _, rec := range h.RunnerKeys.List() {
			if !reachesBinding(all, allowed, rec.Bound) {
				continue
			}
			row := protocol.RunnerKeyRow{EnrolledAtUnixMs: uint64(rec.EnrolledAt.UnixMilli())}
			row.SetBound(rec.Bound)
			copy(row.Key[:], rec.Key)
			row.SetLabel([]byte(rec.Label))
			row.SetEnrolledBy([]byte(rec.EnrolledBy))
			users, _ := h.runnersOnKey(rec.Key)
			row.SetRunners(users)
			rows = append(rows, row)
		}
	}
	body.SetKeys(rows)
	respondWithBody(conn, "runner_keys", body, func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerKeys, RequestId: requestID}
		resp.SetRunnerKeys(protocol.RunnerKeysResponse{RequestId: requestID, StreamId: streamID})
		respondAgent(conn, resp)
	})
}
