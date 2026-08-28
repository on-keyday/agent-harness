package server

import (
	"encoding/hex"
	"log/slog"
	"sort"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// handleBoardTopics returns the agentboard topic overview (metadata only).
// Cap (info_global) is enforced centrally via requiredCap before dispatch.
func (h *TaskHandler) handleBoardTopics(conn ConnHandle, requestID uint32) {
	out := protocol.BoardTopicsResponse{RequestId: requestID}
	for _, r := range h.Board.ListTopics() {
		row := protocol.BoardTopicRow{
			LastSeq:               r.LastSeq,
			LastPublishedAtUnixMs: uint64(r.LastPublishedAt.UnixMilli()),
		}
		row.SetName([]byte(r.Name))
		if r.MsgCount > 65535 {
			row.MsgCount = 65535
		} else {
			row.MsgCount = uint16(r.MsgCount)
		}
		// Withdrawn messages are counted separately, never folded into
		// MsgCount: MsgCount answers "how much would a subscriber receive",
		// which is what every agent-facing path reports.
		if n, found := h.Board.RetractedCount(r.Name); found {
			if n > 65535 {
				row.RetractedCount = 65535
			} else {
				row.RetractedCount = uint16(n)
			}
		}
		out.Topics = append(out.Topics, row)
	}
	out.TopicsLen = uint16(len(out.Topics))
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardTopics, RequestId: requestID}
	resp.SetBoardTopics(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// handleBoardSubscribers reports each task's agentboard subscription set,
// optionally narrowed to the tasks a publish to one topic would reach. Cap
// (board_observe) is enforced centrally via requiredCap before dispatch, matching
// its board_topics / board_read siblings: this is a sweep across every task on
// the board.
//
// Unlike list_conns there is no own-subtree narrowing for confined callers.
// board_topics does not narrow either, and a partial subscriber list would
// answer "is anyone listening" wrongly rather than incompletely.
func (h *TaskHandler) handleBoardSubscribers(conn ConnHandle, requestID uint32, topic string) {
	out := protocol.BoardSubscribersResponse{RequestId: requestID}
	for _, r := range h.Board.ListSubscribers(topic) {
		row := protocol.BoardSubscriberRow{Task: r.Task}
		row.SetHostname([]byte(r.Hostname))
		row.SetAgentProfile([]byte(r.AgentProfile))
		patterns := make([]protocol.SubscriptionPattern, 0, len(r.Patterns))
		for _, p := range r.Patterns {
			var sp protocol.SubscriptionPattern
			sp.SetName([]byte(p.Name))
			// The delivery position, per topic. It used to live in a file on
			// the runner host that no operator surface showed, which is why the
			// shipped skill told agents to guess at a desync and re-read
			// everything.
			sp.Shown = p.Shown
			sp.Pending = p.Pending
			patterns = append(patterns, sp)
		}
		row.SetPatterns(patterns)
		out.Rows = append(out.Rows, row)
	}
	out.RowsLen = uint16(len(out.Rows))
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardSubscribers, RequestId: requestID}
	resp.SetBoardSubscribers(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// handleBoardRead returns metadata for all retained messages in a topic plus a
// server-initiated send-stream carrying the raw payloads in ring order.
// Cap (board_observe) is enforced centrally via requiredCap before dispatch.
//
// Wire shape mirrors handleGetTaskLog: respond first with stream_id, then
// write payloads asynchronously so the metadata response is never blocked by
// stream I/O.
//
// Edge cases:
//   - Topic not found → BoardStatus_NotFound, stream_id 0.
//   - Topic found but ring is empty → BoardStatus_Ok, stream_id 0 (no stream).
//   - conn.CreateSendStream() returns nil (degraded/test path) → metadata only.
func (h *TaskHandler) handleBoardRead(conn ConnHandle, requestID uint32, topic string) {
	respond := func(status protocol.BoardStatus, streamID uint64, rows []protocol.BoardMessageRow) {
		out := protocol.BoardReadResponse{RequestId: requestID, Status: status, StreamId: streamID}
		out.Msgs = rows
		out.MsgsLen = uint16(len(rows))
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardRead, RequestId: requestID}
		resp.SetBoardRead(out)
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}

	msgs, found := h.Board.ListRetained(topic)
	if !found {
		respond(protocol.BoardStatus_NotFound, 0, nil)
		return
	}
	// The operator sees live AND withdrawn messages, in one seq-ordered list.
	// This is the only surface that reads the withdrawn list: an author can
	// retract in seconds, and if that also emptied the operator's view there
	// would be no window left in which to audit what was said. Merging here
	// rather than in the board keeps the storage layer neutral about who may
	// see what (see agentboard.Board.Retained).
	if withdrawn, ok := h.Board.ListRetracted(topic); ok && len(withdrawn) > 0 {
		msgs = append(msgs, withdrawn...)
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
	}
	rows := make([]protocol.BoardMessageRow, 0, len(msgs))
	payloads := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		size := len(m.Payload)
		if size > 0xffffffff {
			size = 0xffffffff
		}
		row := protocol.BoardMessageRow{
			Seq:              m.Seq,
			InReplyTo:        m.InReplyTo,
			ReceivedAtUnixMs: uint64(m.ReceivedAt.UnixMilli()),
			Size:             uint32(size),
			FromTask:         m.FromTask,
		}
		row.SetFromHostname([]byte(m.FromHostname))
		row.SetFromAgentProfile([]byte(m.FromAgentProfile))
		row.SetReplyToTopic([]byte(m.ReplyToTopic))
		if !m.RetractedAt.IsZero() {
			row.SetRetracted(true)
			row.RetractedAtUnixMs = uint64(m.RetractedAt.UnixMilli())
			// Which check let the withdrawal through, and who called. Set only
			// under the retracted bit: on a live row these are the zero value
			// because nothing withdrew it, not because its author did.
			row.RetractedBy = m.RetractedBy
			row.RetractedByTask = m.RetractedByTask
		}
		rows = append(rows, row)
		payloads = append(payloads, m.Payload)
	}
	if len(rows) == 0 {
		respond(protocol.BoardStatus_Ok, 0, rows)
		return
	}
	var stream trsf.SendStream = conn.CreateSendStream()
	if stream == nil {
		// Non-streaming connection (test/degraded): metadata only, no stream.
		respond(protocol.BoardStatus_Ok, 0, rows)
		return
	}
	respond(protocol.BoardStatus_Ok, uint64(stream.ID()), rows)
	go func() {
		for _, p := range payloads {
			if len(p) > 0 {
				if werr := writeStreamAll(stream, p); werr != nil {
					slog.Warn("BoardRead: stream write failed", "topic", topic, "err", werr)
					break
				}
			}
		}
		// Signal EOF on the stream so the client knows we're done.
		if err := stream.AppendData(true); err != nil {
			slog.Warn("BoardRead: stream EOF failed", "topic", topic, "err", err)
		}
	}()
}

// handleBoardRetract withdraws ONE retained message from a topic and leaves it
// readable on the operator surfaces. Cap (purge) enforced centrally — the same
// bit handleBoardPurge takes, because purge already reaches this message and
// destroys it outright; this is the gentler half of that authority, not a new
// one.
//
// by is the caller's principal task, recorded on the withdrawn entry. It is
// zero for an operator client, which holds its capabilities directly and has no
// principal task.
//
// Unknown topic, unknown seq, an already-withdrawn seq and seq 0 all answer
// not_found. Telling them apart would confirm what a topic holds to a caller
// that only guessed at a seq — the same collapse agentboard's RetractStatus
// makes on the authorship path.
func (h *TaskHandler) handleBoardRetract(conn ConnHandle, requestID uint32, topic string, seq uint64, by protocol.TaskID) {
	status := protocol.BoardStatus_NotFound
	if retracted, found := h.Board.ForceRetractSeq(topic, seq, by); found && retracted {
		status = protocol.BoardStatus_Ok
		slog.Info("board retract", "topic", topic, "seq", seq, "by_task", hex.EncodeToString(by.Id[:]))
	}
	out := protocol.BoardRetractResponse{RequestId: requestID, Status: status}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardRetract, RequestId: requestID}
	resp.SetBoardRetract(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// handleBoardPurge drops a topic's ring (seq==0) or a single seq. Cap (purge)
// enforced centrally.
func (h *TaskHandler) handleBoardPurge(conn ConnHandle, requestID uint32, topic string, seq uint64) {
	var status protocol.BoardStatus
	var purged uint16
	if seq == 0 {
		n, found := h.Board.PurgeTopic(topic)
		if !found {
			status = protocol.BoardStatus_NotFound
		} else {
			status = protocol.BoardStatus_Ok
			if n > 65535 {
				purged = 65535
			} else {
				purged = uint16(n)
			}
		}
	} else {
		removed, found := h.Board.PurgeSeq(topic, seq)
		if !found || !removed {
			status = protocol.BoardStatus_NotFound
		} else {
			status = protocol.BoardStatus_Ok
			purged = 1
		}
	}
	out := protocol.BoardPurgeResponse{RequestId: requestID, Status: status, Purged: purged}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardPurge, RequestId: requestID}
	resp.SetBoardPurge(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}
