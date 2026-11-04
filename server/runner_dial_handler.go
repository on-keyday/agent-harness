package server

import (
	"errors"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runner dial add / ls / rm — gated on runner_admin by requiredCap
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md).

const (
	dialMaxCandidates     = 8
	dialMaxCandidateBytes = 512
	dialMaxLabel          = 64
)

func (h *TaskHandler) handleRunnerDialAdd(conn ConnHandle, requestID uint32, r *protocol.RunnerDialAddRequest) {
	status, id := h.runnerDialAdd(conn, r)
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerDialAdd, RequestId: requestID}
	resp.SetRunnerDialAdd(protocol.RunnerDialAddResponse{Status: status, Id: id})
	respondAgent(conn, resp)
}

func (h *TaskHandler) runnerDialAdd(conn ConnHandle, r *protocol.RunnerDialAddRequest) (protocol.RunnerDialStatus, uint32) {
	if h.RunnerDials == nil || h.Dials == nil || h.RunnerKeys == nil {
		return protocol.RunnerDialStatus_InvalidCandidates, 0
	}
	if len(r.Label) > dialMaxLabel || len(r.Candidates) == 0 || len(r.Candidates) > dialMaxCandidates {
		return protocol.RunnerDialStatus_InvalidCandidates, 0
	}
	cands := make([]string, 0, len(r.Candidates))
	total := 0
	for _, c := range r.Candidates {
		text := string(c.Text)
		total += len(text)
		if cli.ParseCandidate(text) != nil {
			return protocol.RunnerDialStatus_InvalidCandidates, 0
		}
		if h.DialLegs == nil || !h.DialLegs(cli.CandidateTransport(text)) {
			return protocol.RunnerDialStatus_InvalidCandidates, 0
		}
		cands = append(cands, text)
	}
	if total > dialMaxCandidateBytes {
		return protocol.RunnerDialStatus_InvalidCandidates, 0
	}
	key, err := h.RunnerKeys.Resolve(string(r.KeyPrefix))
	switch {
	case errors.Is(err, ErrAmbiguousPrefix):
		return protocol.RunnerDialStatus_AmbiguousKey, 0
	case err != nil:
		return protocol.RunnerDialStatus_KeyNotFound, 0
	}
	for _, c := range cands {
		if _, taken := h.RunnerDials.OwnerOfCandidate(c); taken {
			return protocol.RunnerDialStatus_DuplicateCandidate, 0
		}
	}
	by := h.enrolledBy(conn.ConnectionID().String())
	rec, err := h.RunnerDials.Add(RunnerDialRecord{Candidates: cands, Key: key.Key,
		Label: string(r.Label), AddedAt: time.Now().UTC(), AddedBy: by})
	if err != nil {
		slog.Error("runner dial add: store write failed", "err", err)
		return protocol.RunnerDialStatus_StoreFailed, 0
	}
	slog.Info("dial record added", "record", FormatDialRecordID(rec.ID), "candidates", cands,
		"key", keyPrefix(key.Key), "by", by)
	h.Dials.Add(rec)
	return protocol.RunnerDialStatus_Ok, rec.ID
}

func (h *TaskHandler) handleRunnerDialList(conn ConnHandle, requestID uint32) {
	body := &protocol.RunnerDialListBody{}
	if h.Dials != nil {
		body.SetRows(h.Dials.States())
	}
	respondWithBody(conn, "runner_dial_list", body, func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerDialList, RequestId: requestID}
		resp.SetRunnerDialList(protocol.RunnerDialListResponse{RequestId: requestID, StreamId: streamID})
		respondAgent(conn, resp)
	})
}

func (h *TaskHandler) handleRunnerDialRemove(conn ConnHandle, requestID uint32, r *protocol.RunnerDialRemoveRequest) {
	status := protocol.RunnerDialStatus_Ok
	if h.RunnerDials == nil {
		status = protocol.RunnerDialStatus_NotFound
	} else if _, err := h.RunnerDials.Remove(r.Id); errors.Is(err, ErrDialNotFound) {
		status = protocol.RunnerDialStatus_NotFound
	} else if err != nil {
		slog.Error("runner dial rm: store write failed; the record stays", "record", FormatDialRecordID(r.Id), "err", err)
		status = protocol.RunnerDialStatus_StoreFailed
	} else {
		// The live connection stays (D17): runner kick is what disconnects.
		if h.Dials != nil {
			h.Dials.Stop(r.Id)
		}
		slog.Info("dial record removed", "record", FormatDialRecordID(r.Id),
			"by", h.enrolledBy(conn.ConnectionID().String()))
	}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RunnerDialRemove, RequestId: requestID}
	resp.SetRunnerDialRemove(protocol.RunnerDialRemoveResponse{Status: status})
	respondAgent(conn, resp)
}

// removeDialsOfKey is runner unenroll's cascade (D5): every record bound to the
// key goes with it, loop and all. Returns the removed ids.
func (h *TaskHandler) removeDialsOfKey(rec RunnerKeyRecord) []uint32 {
	if h.RunnerDials == nil {
		return nil
	}
	var ids []uint32
	for _, d := range h.RunnerDials.RemoveByKey(rec.Key) {
		if h.Dials != nil {
			h.Dials.Stop(d.ID)
		}
		ids = append(ids, d.ID)
	}
	return ids
}
