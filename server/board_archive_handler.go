package server

import (
	"errors"
	"log/slog"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Flush and archives. See
// docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.

// handleBoardFlush empties a retained topic's ring. Cap (board_persist) is
// enforced centrally; a task caller must also be able to publish to the topic,
// as for board_retain.
func (h *TaskHandler) handleBoardFlush(conn ConnHandle, requestID uint32, topic string, by protocol.TaskID) {
	out := protocol.BoardFlushResponse{Status: protocol.BoardFlushStatus_StoreError}
	defer func() {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardFlush, RequestId: requestID}
		resp.SetBoardFlush(out)
		respondAgent(conn, resp)
	}()
	if h.Board == nil {
		return
	}
	if by != (protocol.TaskID{}) && !h.Board.PublishableBy(by, topic) {
		out.Status = protocol.BoardFlushStatus_NotPublishable
		return
	}
	through, dropped, err := h.Board.Flush(topic)
	switch {
	case err == nil:
		out.Status, out.ThroughSeq, out.Dropped = protocol.BoardFlushStatus_Ok, through, clampU16(dropped)
	case errors.Is(err, agentboard.ErrNoTopic):
		out.Status = protocol.BoardFlushStatus_NotFound
	case errors.Is(err, agentboard.ErrNotRetained):
		out.Status = protocol.BoardFlushStatus_NotRetained
	default:
		slog.Error("board_flush", "topic", topic, "err", err)
	}
}

// handleBoardArchive freezes a topic's history into an archive. Gated like
// handleBoardFlush.
func (h *TaskHandler) handleBoardArchive(conn ConnHandle, requestID uint32, topic string, by protocol.TaskID) {
	out := protocol.BoardArchiveResponse{Status: protocol.BoardArchiveStatus_StoreError}
	defer func() {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardArchive, RequestId: requestID}
		resp.SetBoardArchive(out)
		respondAgent(conn, resp)
	}()
	if h.Board == nil {
		return
	}
	if !agentboard.Retainable(topic) {
		out.Status = protocol.BoardArchiveStatus_NotArchivable
		return
	}
	if by != (protocol.TaskID{}) && !h.Board.PublishableBy(by, topic) {
		out.Status = protocol.BoardArchiveStatus_NotPublishable
		return
	}
	info, err := h.Board.Archive(topic, by)
	switch {
	case err == nil:
		out.Status, out.ThroughSeq, out.Count = protocol.BoardArchiveStatus_Ok, info.Through, uint32(info.Count)
		out.SetId([]byte(info.ID))
	case errors.Is(err, agentboard.ErrNoTopic):
		out.Status = protocol.BoardArchiveStatus_NotFound
	case errors.Is(err, agentboard.ErrNotRetainable):
		out.Status = protocol.BoardArchiveStatus_NotArchivable
	case errors.Is(err, agentboard.ErrArchiveEmpty):
		out.Status = protocol.BoardArchiveStatus_Empty
	default:
		slog.Error("board_archive", "topic", topic, "err", err)
	}
}

// handleBoardArchives lists archives as rows on a stream. kind says which face
// asked: board_archives (cap board_observe, any topic or all) or
// agent_archives (checked by the caller before this).
func (h *TaskHandler) handleBoardArchives(conn ConnHandle, requestID uint32, kind protocol.TaskControlKind, topic string) {
	respond := func(status protocol.BoardStatus) func(uint64) {
		return func(streamID uint64) {
			out := protocol.BoardArchivesResponse{RequestId: requestID, Status: status, StreamId: streamID}
			resp := protocol.TaskControlResponse{Kind: kind, RequestId: requestID}
			if kind == protocol.TaskControlKind_AgentArchives {
				resp.SetAgentArchives(out)
			} else {
				resp.SetBoardArchives(out)
			}
			respondAgent(conn, resp)
		}
	}
	if h.Board == nil {
		respond(protocol.BoardStatus_StoreError)(0)
		return
	}
	list, err := h.Board.Archives(topic)
	if err != nil && !errors.Is(err, agentboard.ErrNoStore) {
		slog.Error("board_archives", "topic", topic, "err", err)
		respond(protocol.BoardStatus_StoreError)(0)
		return
	}
	rows := make([]protocol.BoardArchiveRow, 0, len(list))
	for _, a := range list {
		row := protocol.BoardArchiveRow{ThroughSeq: a.Through, Count: uint32(a.Count),
			CreatedAtUnixMs: uint64(a.CreatedAt.UnixMilli()), CreatedBy: a.CreatedBy, DiskBytes: uint64(a.Bytes)}
		row.SetId([]byte(a.ID))
		row.SetTopic([]byte(a.Topic))
		rows = append(rows, row)
	}
	body := &protocol.BoardArchivesBody{}
	body.SetRows(rows)
	respondWithBody(conn, "board_archives", body, respond(protocol.BoardStatus_Ok))
}

// handleBoardArchiveRead pages an archive for the operator face: withdrawn
// messages included, as board_read. Cap (board_observe) enforced centrally.
func (h *TaskHandler) handleBoardArchiveRead(conn ConnHandle, requestID uint32, id string, beforeSeq uint64, limit uint16) {
	respond := func(status protocol.BoardStatus) func(uint64) {
		return func(streamID uint64) {
			resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardArchiveRead, RequestId: requestID}
			resp.SetBoardArchiveRead(protocol.BoardReadResponse{RequestId: requestID, Status: status, StreamId: streamID})
			respondAgent(conn, resp)
		}
	}
	if h.Board == nil || limit == 0 {
		respond(protocol.BoardStatus_NotFound)(0)
		return
	}
	if _, ok, err := h.Board.ArchiveOf(id); err != nil || !ok {
		if err != nil {
			slog.Error("board_archive_read", "id", id, "err", err)
		}
		respond(protocol.BoardStatus_NotFound)(0)
		return
	}
	page, err := h.Board.ArchivePage(id, beforeSeq, int(limit))
	if err != nil {
		slog.Error("board_archive_read: page", "id", id, "err", err)
		respond(protocol.BoardStatus_StoreError)(0)
		return
	}
	body := &protocol.BoardReadBody{}
	body.SetMsgs(boardMessageRows(page))
	respondWithBody(conn, "board_archive_read", body, respond(protocol.BoardStatus_Ok))
}

// handleBoardArchivePurge deletes one message of an archive, or the archive
// for seq 0. Cap (board_purge) enforced centrally. purged is the messages
// destroyed.
func (h *TaskHandler) handleBoardArchivePurge(conn ConnHandle, requestID uint32, id string, seq uint64) {
	out := protocol.BoardPurgeResponse{RequestId: requestID, Status: protocol.BoardStatus_NotFound}
	defer func() {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardArchivePurge, RequestId: requestID}
		resp.SetBoardArchivePurge(out)
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}()
	if h.Board == nil {
		return
	}
	info, ok, err := h.Board.ArchiveOf(id)
	if err != nil || !ok {
		return
	}
	removed, _, err := h.Board.ArchivePurge(id, seq)
	switch {
	case err != nil:
		slog.Error("board_archive_purge", "id", id, "seq", seq, "err", err)
		out.Status = protocol.BoardStatus_StoreError
	case !removed:
	case seq == 0:
		out.Status, out.Purged = protocol.BoardStatus_Ok, clampU16(info.Count)
	default:
		out.Status, out.Purged = protocol.BoardStatus_Ok, 1
	}
}

// handleAgentArchives lists the archives of one topic the caller names and
// may read. No cap: a keyed read, which an unreadable or empty topic answers
// with the same not_found.
func (h *TaskHandler) handleAgentArchives(conn ConnHandle, requestID uint32, topic string) {
	st := h.boardState(conn)
	if topic == "" || h.Board == nil || st == nil || !h.Board.Readable(st, topic) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentArchives, RequestId: requestID}
		resp.SetAgentArchives(protocol.BoardArchivesResponse{RequestId: requestID, Status: protocol.BoardStatus_NotFound})
		respondAgent(conn, resp)
		return
	}
	h.handleBoardArchives(conn, requestID, protocol.TaskControlKind_AgentArchives, topic)
}

// handleAgentArchiveRead pages an archive whose source topic the caller may
// read, live messages only. No cap; an unknown id and an unreadable archive
// answer the same not_found, as agent_read_page does.
func (h *TaskHandler) handleAgentArchiveRead(conn ConnHandle, requestID uint32, r *protocol.AgentArchiveReadRequest) {
	out := protocol.AgentReadPageResponse{RequestId: requestID, Status: protocol.AgentReadPageStatus_NotFound}
	respond := func(streamID uint64) {
		out.StreamId = streamID
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentArchiveRead, RequestId: requestID}
		resp.SetAgentArchiveRead(out)
		respondAgent(conn, resp)
	}
	id := string(r.Id)
	st := h.boardState(conn)
	if st == nil || h.Board == nil || r.Limit == 0 {
		respond(0)
		return
	}
	info, ok, err := h.Board.ArchiveOf(id)
	if err != nil || !ok || !h.Board.Readable(st, info.Topic) {
		respond(0)
		return
	}
	page, err := h.Board.ArchivePageLive(id, r.BeforeSeq, int(r.Limit))
	if err != nil {
		slog.Error("agent_archive_read: page", "id", id, "err", err)
		out.Status = protocol.AgentReadPageStatus_StoreError
		respond(0)
		return
	}
	out.Status = protocol.AgentReadPageStatus_Ok
	respondWithBody(conn, "archive_read", deliveredBody(h.Board, st, page), respond)
}

func clampU16(n int) uint16 {
	if n > 65535 {
		return 65535
	}
	return uint16(n)
}
