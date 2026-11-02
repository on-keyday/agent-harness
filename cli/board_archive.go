package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Flush and archives: see
// docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.

// BoardArchiveRow is one archive as board archives / agent archives list it.
type BoardArchiveRow struct {
	ID        string
	Topic     string
	Through   uint64 // the highest seq it holds
	Count     int    // messages, live and withdrawn
	CreatedAt time.Time
	// CreatedByHex is the task that made it; empty means the operator.
	CreatedByHex string
	Bytes        uint64
}

// ByLabel is the creator's short id, or "operator".
func (r BoardArchiveRow) ByLabel() string {
	if r.CreatedByHex == "" {
		return "operator"
	}
	return shortID(r.CreatedByHex)
}

// Summary is one archive in board archives' words —
// "<id> topic=<t> through=<seq> msgs=<n> by=<who> disk=<bytes> at=<time>" —
// the one spelling the CLI, the TUI and the WebUI print.
func (r BoardArchiveRow) Summary() string {
	return fmt.Sprintf("%s topic=%s through=%d msgs=%d by=%s disk=%d at=%s",
		r.ID, r.Topic, r.Through, r.Count, r.ByLabel(), r.Bytes, r.CreatedAt.UTC().Format(time.RFC3339))
}

// JSON is the row as one JSON Lines record, the shape both faces print.
func (r BoardArchiveRow) JSON() map[string]any {
	return map[string]any{
		"id": r.ID, "topic": r.Topic, "through": r.Through, "msgs": r.Count,
		"by": r.ByLabel(), "disk_bytes": r.Bytes, "at": r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// ArchiveRows converts the wire rows.
func ArchiveRows(rows []protocol.BoardArchiveRow) []BoardArchiveRow {
	out := make([]BoardArchiveRow, 0, len(rows))
	for _, r := range rows {
		row := BoardArchiveRow{ID: string(r.Id), Topic: string(r.Topic), Through: r.ThroughSeq, Count: int(r.Count),
			CreatedAt: time.UnixMilli(int64(r.CreatedAtUnixMs)).UTC(), Bytes: r.DiskBytes}
		if r.CreatedBy.Id != ([16]byte{}) {
			row.CreatedByHex = hex.EncodeToString(r.CreatedBy.Id[:])
		}
		out = append(out, row)
	}
	return out
}

// BoardFlush empties a retained topic's ring; its disk log keeps everything.
// Requires Capability_BoardPersist, and a task caller may flush only a topic
// it may publish to. A non-ok status is returned as an error naming it.
func (c *Client) BoardFlush(ctx context.Context, topic string) (through uint64, dropped int, err error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardFlush}
	var fr protocol.BoardFlushRequest
	if !fr.SetTopic([]byte(topic)) {
		return 0, 0, errors.New("board flush: topic too long")
	}
	req.SetBoardFlush(fr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, 0, err
	}
	r := resp.BoardFlush()
	if r == nil || resp.Kind != protocol.TaskControlKind_BoardFlush {
		return 0, 0, fmt.Errorf("BoardFlush: unexpected response kind=%v", resp.Kind)
	}
	if r.Status != protocol.BoardFlushStatus_Ok {
		return 0, 0, fmt.Errorf("board flush %s: %s", topic, r.Status)
	}
	return r.ThroughSeq, int(r.Dropped), nil
}

// BoardArchive freezes topic's history into an archive. Gated like BoardFlush.
func (c *Client) BoardArchive(ctx context.Context, topic string) (BoardArchiveRow, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchive}
	var ar protocol.BoardArchiveRequest
	if !ar.SetTopic([]byte(topic)) {
		return BoardArchiveRow{}, errors.New("board archive: topic too long")
	}
	req.SetBoardArchive(ar)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return BoardArchiveRow{}, err
	}
	r := resp.BoardArchive()
	if r == nil || resp.Kind != protocol.TaskControlKind_BoardArchive {
		return BoardArchiveRow{}, fmt.Errorf("BoardArchive: unexpected response kind=%v", resp.Kind)
	}
	if r.Status != protocol.BoardArchiveStatus_Ok {
		return BoardArchiveRow{}, fmt.Errorf("board archive %s: %s", topic, r.Status)
	}
	return BoardArchiveRow{ID: string(r.Id), Topic: topic, Through: r.ThroughSeq, Count: int(r.Count)}, nil
}

// BoardArchives lists archives of topic, or every archive for "". Requires
// Capability_BoardObserve.
func (c *Client) BoardArchives(ctx context.Context, topic string) ([]BoardArchiveRow, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchives}
	var lr protocol.BoardArchivesRequest
	if !lr.SetTopic([]byte(topic)) {
		return nil, errors.New("board archives: topic too long")
	}
	req.SetBoardArchives(lr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	r := resp.BoardArchives()
	if r == nil || resp.Kind != protocol.TaskControlKind_BoardArchives {
		return nil, fmt.Errorf("BoardArchives: unexpected response kind=%v", resp.Kind)
	}
	if r.Status != protocol.BoardStatus_Ok {
		return nil, fmt.Errorf("board archives: %s (see the server log)", r.Status)
	}
	var body protocol.BoardArchivesBody
	if err := ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return nil, fmt.Errorf("BoardArchives: %w", err)
	}
	return ArchiveRows(body.Rows), nil
}

// BoardArchiveRead pages an archive, withdrawn messages included, newest
// first: up to limit with seq below beforeSeq (0 = from the newest). found is
// false for an unknown archive. Requires Capability_BoardObserve.
func (c *Client) BoardArchiveRead(ctx context.Context, id string, beforeSeq uint64, limit uint16) ([]BoardMessage, bool, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchiveRead}
	rr := protocol.BoardArchiveReadRequest{BeforeSeq: beforeSeq, Limit: limit}
	if !rr.SetId([]byte(id)) {
		return nil, false, errors.New("board read --archive: id too long")
	}
	req.SetBoardArchiveRead(rr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, false, err
	}
	r := resp.BoardArchiveRead()
	if r == nil || resp.Kind != protocol.TaskControlKind_BoardArchiveRead {
		return nil, false, fmt.Errorf("BoardArchiveRead: unexpected response kind=%v", resp.Kind)
	}
	switch r.Status {
	case protocol.BoardStatus_NotFound:
		return nil, false, nil
	case protocol.BoardStatus_Ok:
	default:
		return nil, true, fmt.Errorf("board read --archive %s: %s (see the server log)", id, r.Status)
	}
	var body protocol.BoardReadBody
	if err := ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return nil, true, fmt.Errorf("BoardArchiveRead: %w", err)
	}
	return boardMessages(body.Msgs), true, nil
}

// BoardArchivePurge deletes one message of an archive, or the whole archive
// for seq 0. found is false when the archive, or that seq in it, does not
// exist. Requires Capability_BoardPurge.
func (c *Client) BoardArchivePurge(ctx context.Context, id string, seq uint64) (purged int, found bool, err error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchivePurge}
	pr := protocol.BoardArchivePurgeRequest{Seq: seq}
	if !pr.SetId([]byte(id)) {
		return 0, false, errors.New("purge --archive: id too long")
	}
	req.SetBoardArchivePurge(pr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, false, err
	}
	r := resp.BoardArchivePurge()
	if r == nil || resp.Kind != protocol.TaskControlKind_BoardArchivePurge {
		return 0, false, fmt.Errorf("BoardArchivePurge: unexpected response kind=%v", resp.Kind)
	}
	switch r.Status {
	case protocol.BoardStatus_Ok:
		return int(r.Purged), true, nil
	case protocol.BoardStatus_NotFound:
		return 0, false, nil
	default:
		return 0, true, fmt.Errorf("purge --archive %s: %s (see the server log)", id, r.Status)
	}
}

// The package-level fresh-dial wrappers, as BoardRetain's.

func BoardFlush(ctx context.Context, peerCID objproto.ConnectionID, topic string) (uint64, int, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return 0, 0, err
	}
	defer c.Close()
	return c.BoardFlush(ctx, topic)
}

func BoardArchive(ctx context.Context, peerCID objproto.ConnectionID, topic string) (BoardArchiveRow, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return BoardArchiveRow{}, err
	}
	defer c.Close()
	return c.BoardArchive(ctx, topic)
}

func BoardArchives(ctx context.Context, peerCID objproto.ConnectionID, topic string) ([]BoardArchiveRow, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.BoardArchives(ctx, topic)
}

func BoardArchiveRead(ctx context.Context, peerCID objproto.ConnectionID, id string, beforeSeq uint64, limit uint16) ([]BoardMessage, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, false, err
	}
	defer c.Close()
	return c.BoardArchiveRead(ctx, id, beforeSeq, limit)
}

func BoardArchivePurge(ctx context.Context, peerCID objproto.ConnectionID, id string, seq uint64) (int, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return 0, false, err
	}
	defer c.Close()
	return c.BoardArchivePurge(ctx, id, seq)
}
