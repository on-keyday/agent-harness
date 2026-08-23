package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// BoardTopicRow holds metadata for one topic returned by BoardTopics.
type BoardTopicRow struct {
	Name              string
	LastSeq           uint64
	LastPublishedAtMs uint64
	MsgCount          int
	// RetractedCount is how many withdrawn messages the topic still holds for
	// operator audit. Kept out of MsgCount, which answers "how much would a
	// subscriber receive".
	RetractedCount int
}

// BoardMessage holds one retained message returned by BoardRead.
// Payload is the raw bytes of the message as stored in the board ring.
type BoardMessage struct {
	Seq uint64
	// InReplyTo is the seq of the message this one answers, 0 when it is not a
	// reply. See agentboard/agentboard.bgn SendRequest.in_reply_to.
	InReplyTo    uint64
	FromTaskHex  string
	FromHostname string
	// FromAgentProfile is the agent profile the sender was running under when
	// the message was published. Empty = the server could not attribute a
	// runtime (e.g. a server-originated publish, which carries FromHostname
	// "server").
	FromAgentProfile string
	// ReplyToTopic is where a reply to THIS message is routed -- the
	// destination its SENDER declared with `agent send --reply-to`. Empty =
	// none declared, so a reply comes back to the sender's own
	// chat.<short-id>. It is the only place an operator can see that routing:
	// the server resolves it off the parent, and neither message's text
	// mentions it.
	ReplyToTopic string
	ReceivedAtMs uint64
	Payload      []byte
	// Retracted is true when the message's author withdrew it (agent retract).
	// A withdrawn message is invisible to every agent-facing path and reaches
	// only this operator view, so a retract at agent speed cannot shrink the
	// window a human has to audit what was said. RetractedAtMs is 0 unless
	// Retracted is true.
	Retracted     bool
	RetractedAtMs uint64
}

// BoardSubscriberRow is one task's agentboard subscription set as returned by
// BoardSubscribers. Hostname is empty for a task that has been registered (so
// its chat.<short-id> is seeded) but has not yet run a harness-cli command —
// a real state, not missing data.
type BoardSubscriberRow struct {
	TaskHex      string
	Hostname     string
	AgentProfile string
	Patterns     []BoardSubscriberPattern
}

// BoardSubscriberPattern is one subscribed topic plus this task's delivery
// position on it. Shown is the highest seq the automatic injection path has
// given the task; Pending is how many retained messages sit above it. Both are
// zero for a topic nothing has been published to. See
// agentboard.SubscriberPattern.
type BoardSubscriberPattern struct {
	Name    string
	Shown   uint64
	Pending uint32
}

// BoardSubscribers lists each task's agentboard subscription set. A non-empty
// topic narrows the result to the tasks a publish to that topic would reach;
// each returned row still carries its full pattern set. Requires
// Capability_BoardObserve, like BoardTopics / BoardRead.
//
// Rows are sorted by task id here rather than on the board: Board.ListSubscribers
// iterates a map and declares its order unspecified, and stable output is a
// property the CLI and its tests want, not one the board should have to promise.
func (c *Client) BoardSubscribers(ctx context.Context, topic string) ([]BoardSubscriberRow, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardSubscribers}
	sr := protocol.BoardSubscribersRequest{}
	sr.SetTopic([]byte(topic))
	req.SetBoardSubscribers(sr)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	bs := resp.BoardSubscribers()
	if bs == nil || resp.Kind != protocol.TaskControlKind_BoardSubscribers {
		return nil, fmt.Errorf("BoardSubscribers: unexpected response kind=%v", resp.Kind)
	}
	out := make([]BoardSubscriberRow, 0, len(bs.Rows))
	for _, r := range bs.Rows {
		patterns := make([]BoardSubscriberPattern, 0, len(r.Patterns))
		for _, p := range r.Patterns {
			patterns = append(patterns, BoardSubscriberPattern{
				Name:    string(p.Name),
				Shown:   p.Shown,
				Pending: p.Pending,
			})
		}
		sort.Slice(patterns, func(i, j int) bool { return patterns[i].Name < patterns[j].Name })
		out = append(out, BoardSubscriberRow{
			TaskHex:      hex.EncodeToString(r.Task.Id[:]),
			Hostname:     string(r.Hostname),
			AgentProfile: string(r.AgentProfile),
			Patterns:     patterns,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskHex < out[j].TaskHex })
	return out, nil
}

// ShownTo reports how many of a topic's subscribers have already been handed
// the message with this seq by the automatic injection path, out of how many
// subscribe to the topic at all.
//
// It is derived rather than stored: the server keeps ONE watermark per
// (task, topic), so "has this particular message been shown" is
// `subscriber.Shown >= seq`. Deriving it here, once, is deliberate — the
// alternative is each surface comparing 19-digit board seqs itself, and the
// browser would have to do it in BigInt because those exceed
// Number.MAX_SAFE_INTEGER. The wasm bridge therefore ships the RESULT of this
// function rather than the inputs.
//
// total counts the topic's subscribers, so `0/0` (nobody subscribes) is
// distinguishable from `0/1` (someone does and has not been handed it). Both
// are printed by callers: zero is a measurement.
func ShownTo(subs []BoardSubscriberRow, topic string, seq uint64) (n, total int) {
	for _, r := range subs {
		for _, p := range r.Patterns {
			if p.Name != topic {
				continue
			}
			total++
			if p.Shown >= seq {
				n++
			}
		}
	}
	return n, total
}

// ShownToLabel renders ShownTo for a text row. One spelling, so the CLI and the
// TUI cannot drift apart on it.
func ShownToLabel(subs []BoardSubscriberRow, topic string, seq uint64) string {
	n, total := ShownTo(subs, topic, seq)
	return fmt.Sprintf("shown_to=%d/%d", n, total)
}

// BoardTopics lists every topic currently held in the board with aggregate
// metadata (last seq, last publish time, message count). Requires the caller
// to hold Capability_BoardObserve; operator connections (ClientKind_Cli with no
// principal task) hold Capability_All and pass this gate unconditionally.
func (c *Client) BoardTopics(ctx context.Context) ([]BoardTopicRow, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardTopics}
	req.SetBoardTopics(protocol.BoardTopicsRequest{})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	bt := resp.BoardTopics()
	if bt == nil || resp.Kind != protocol.TaskControlKind_BoardTopics {
		return nil, fmt.Errorf("BoardTopics: unexpected response kind=%v", resp.Kind)
	}
	out := make([]BoardTopicRow, 0, len(bt.Topics))
	for _, r := range bt.Topics {
		out = append(out, BoardTopicRow{
			Name:              string(r.Name),
			LastSeq:           r.LastSeq,
			LastPublishedAtMs: r.LastPublishedAtUnixMs,
			MsgCount:          int(r.MsgCount),
			RetractedCount:    int(r.RetractedCount),
		})
	}
	return out, nil
}

// BoardRead returns all retained messages for the named topic.
// bool=false (not found) when the topic does not exist.
// Payloads are retrieved from a server-initiated trsf send-stream and sliced
// into per-message []byte values using the Size field of each metadata row.
// The pattern mirrors (*Client).GetTaskLog: send request, receive metadata
// response with a stream_id, then drain the stream until EOF.
func (c *Client) BoardRead(ctx context.Context, topic string) ([]BoardMessage, bool, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardRead}
	rr := protocol.BoardReadRequest{}
	rr.SetTopic([]byte(topic))
	req.SetBoardRead(rr)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, false, err
	}
	br := resp.BoardRead()
	if br == nil || resp.Kind != protocol.TaskControlKind_BoardRead {
		return nil, false, fmt.Errorf("BoardRead: unexpected response kind=%v", resp.Kind)
	}
	if br.Status == protocol.BoardStatus_NotFound {
		return nil, false, nil
	}

	rows := make([]BoardMessage, len(br.Msgs))
	total := 0
	for i, m := range br.Msgs {
		rows[i] = BoardMessage{
			Seq:              m.Seq,
			InReplyTo:        m.InReplyTo,
			FromTaskHex:      hex.EncodeToString(m.FromTask.Id[:]),
			FromHostname:     string(m.FromHostname),
			FromAgentProfile: string(m.FromAgentProfile),
			ReplyToTopic:     string(m.ReplyToTopic),
			ReceivedAtMs:     m.ReceivedAtUnixMs,
			Retracted:        m.Retracted(),
			RetractedAtMs:    m.RetractedAtUnixMs,
		}
		total += int(m.Size)
	}

	if br.StreamId != 0 {
		st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(br.StreamId))
		if st == nil {
			return nil, true, fmt.Errorf("BoardRead: stream %d not visible after response", br.StreamId)
		}
		buf := make([]byte, 0, total)
		for {
			select {
			case <-ctx.Done():
				return nil, true, ctx.Err()
			default:
			}
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return nil, true, fmt.Errorf("BoardRead: stream read: %w", err)
			}
			buf = append(buf, data...)
			if eof {
				break
			}
		}
		// Slice the concatenated stream bytes by each row's Size, in order.
		off := 0
		for i := range rows {
			n := int(br.Msgs[i].Size)
			if off+n > len(buf) {
				n = len(buf) - off
			}
			rows[i].Payload = append([]byte(nil), buf[off:off+n]...)
			off += n
		}
	}
	return rows, true, nil
}

// BoardPurge drops one retained message (seq != 0) or the entire topic ring
// (seq == 0). Returns (purged count, found, error).
// found=false when the topic (or the specific seq) does not exist.
func (c *Client) BoardPurge(ctx context.Context, topic string, seq uint64) (int, bool, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardPurge}
	pr := protocol.BoardPurgeRequest{Seq: seq}
	pr.SetTopic([]byte(topic))
	req.SetBoardPurge(pr)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, false, err
	}
	bp := resp.BoardPurge()
	if bp == nil || resp.Kind != protocol.TaskControlKind_BoardPurge {
		return 0, false, fmt.Errorf("BoardPurge: unexpected response kind=%v", resp.Kind)
	}
	if bp.Status == protocol.BoardStatus_NotFound {
		return 0, false, nil
	}
	return int(bp.Purged), true, nil
}

// BoardTopics is a package-level fresh-dial wrapper: it opens a new Client,
// calls (*Client).BoardTopics, and closes the connection. Suitable for
// short-lived CLI processes; long-lived consumers should hold a *Client.
func BoardTopics(ctx context.Context, peerCID objproto.ConnectionID) ([]BoardTopicRow, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.BoardTopics(ctx)
}

// BoardRead is a package-level fresh-dial wrapper for (*Client).BoardRead.
func BoardRead(ctx context.Context, peerCID objproto.ConnectionID, topic string) ([]BoardMessage, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, false, err
	}
	defer c.Close()
	return c.BoardRead(ctx, topic)
}

// BoardSubscribers is a package-level fresh-dial wrapper for
// (*Client).BoardSubscribers.
func BoardSubscribers(ctx context.Context, peerCID objproto.ConnectionID, topic string) ([]BoardSubscriberRow, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.BoardSubscribers(ctx, topic)
}

// BoardPurge is a package-level fresh-dial wrapper for (*Client).BoardPurge.
func BoardPurge(ctx context.Context, peerCID objproto.ConnectionID, topic string, seq uint64) (int, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return 0, false, err
	}
	defer c.Close()
	return c.BoardPurge(ctx, topic, seq)
}
