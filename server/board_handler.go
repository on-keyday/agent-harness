package server

import (
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
		out.Topics = append(out.Topics, row)
	}
	out.TopicsLen = uint16(len(out.Topics))
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardTopics, RequestId: requestID}
	resp.SetBoardTopics(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// handleBoardRead returns metadata for all retained messages in a topic plus a
// server-initiated send-stream carrying the raw payloads in ring order.
// Cap (InfoGlobal) is enforced centrally via requiredCap before dispatch.
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
	rows := make([]protocol.BoardMessageRow, 0, len(msgs))
	payloads := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		size := len(m.Payload)
		if size > 0xffffffff {
			size = 0xffffffff
		}
		row := protocol.BoardMessageRow{
			Seq:              m.Seq,
			ReceivedAtUnixMs: uint64(m.ReceivedAt.UnixMilli()),
			Size:             uint32(size),
			FromTask:         m.FromTask,
		}
		row.SetFromHostname([]byte(m.FromHostname))
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
		defer stream.Close()
		for _, p := range payloads {
			if len(p) > 0 {
				_ = writeStreamAll(stream, p)
			}
		}
	}()
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
