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
// on runner_enroll by requiredCap before it reaches here.

// enrolledBy names who enrolled a key: the caller's principal task, or the
// operator for a connection that has none.
func (h *TaskHandler) enrolledBy(cid string) string {
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
		added, err := h.RunnerKeys.Enroll(RunnerKeyRecord{
			Key: key, Label: string(r.Label), EnrolledAt: time.Now().UTC(),
			EnrolledBy: h.enrolledBy(conn.ConnectionID().String()),
		})
		if err != nil {
			slog.Error("runner enroll: store write failed", "err", err)
			status = protocol.RunnerKeyStatus_InvalidKey
		} else if !added {
			status = protocol.RunnerKeyStatus_AlreadyEnrolled
		} else {
			slog.Info("runner key enrolled", "key", keyPrefix(key), "label", string(r.Label),
				"by", h.enrolledBy(conn.ConnectionID().String()))
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
	rec, err := h.RunnerKeys.Resolve(prefix)
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
	slog.Warn("runner key unenrolled", "key", keyPrefix(rec.Key), "label", rec.Label,
		"by", h.enrolledBy(conn.ConnectionID().String()), "closing_runners", len(conns))
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
		for _, rec := range h.RunnerKeys.List() {
			row := protocol.RunnerKeyRow{EnrolledAtUnixMs: uint64(rec.EnrolledAt.UnixMilli())}
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
