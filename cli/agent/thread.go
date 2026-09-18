package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// Thread is the entry for `harness-cli agent thread`: the calling task's side
// of the reply chain, assembled from the topics IT subscribes to.
//
// This verb exists because the operator face (`board thread`) is gated on
// board_observe, and a worker spawned with --caps omitted holds nothing —
// which is the common case. The agent-side reads are ungated
// (server/agent_handler.go, where the rationale is recorded), so this face
// collects from the agent's OWN subscriptions and never enumerates the board.
// If you find yourself calling ListTopics here, you have rebuilt the operator
// verb and the server will deny it at runtime.
//
// The consequence is stated on the surface, not hidden: a chain whose other
// half sits on a peer's topic renders as an ORPHAN-rooted fragment, which is
// what this task can actually see — a truncated conversation here is the
// view working, not the viewer failing.
func Thread(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("thread", args)
	if perr != nil {
		return perr
	}
	return ThreadWith(ctx, a, stdout)
}

// ThreadWith is Thread for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func ThreadWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	conn, err := ConnectAgent(ctx, Flags{ServerCID: a.ServerCID})
	if err != nil {
		return err
	}
	defer conn.Close()

	// 1. The topic set: what THIS task subscribes to. No enumeration.
	topics, err := listSubscribedTopics(ctx, conn)
	if err != nil {
		return err
	}

	// 2. The messages: retained metadata per topic (which is where the
	// received-at timestamps live), payloads afterwards per seq via the read
	// path — the same scoped read `agent read` uses, so the body of a message
	// on a topic this task subscribes to needs no capability either.
	var msgs []cli.BoardMessage
	topicOf := make(map[uint64]string)
	sizes := make(map[uint64]int) // seq -> published size, from the metas
	for _, topic := range topics {
		metas, tsizes, err := retainedMetas(ctx, conn, topic)
		if err != nil {
			return err
		}
		for _, m := range metas {
			topicOf[m.Seq] = topic
			msgs = append(msgs, m)
		}
		for seq, n := range tsizes {
			sizes[seq] = n
		}
	}

	rows, serr := cli.SelectThreads(cli.BuildThreads(msgs, topicOf), topicOf, cli.ThreadFilter{
		Tasks: a.Tasks,
		Seq:   a.Seq,
	})
	if serr != nil {
		var e *cli.SeqNotVisibleError
		if errors.As(serr, &e) {
			return fmt.Errorf("agent thread: seq %d is not readable from this task: its topic may have died with its last subscriber task, it may have rotated out of a 64-message ring, or it sits on a topic this task does not subscribe to", e.Seq)
		}
		return serr
	}

	// 3. Carry the published sizes onto the rows: under --headers-only the
	// body is never fetched, and the renderer must not print 0 and claim a
	// zero-byte message was published.
	for i := range rows {
		rows[i].Size = sizes[rows[i].Msg.Seq]
	}

	// 4. Bodies, unless suppressed. --json still carries payload_b64, so only
	// --headers-only skips the fetches.
	if !a.HeadersOnly {
		for i := range rows {
			payload, err := fetchBody(ctx, conn, rows[i].Msg.Seq)
			if err != nil {
				return err
			}
			rows[i].Msg.Payload = payload
		}
	}

	return cli.RenderThreads(stdout, rows, cli.ThreadRenderOptions{
		JSON:        a.JSON,
		HeadersOnly: a.HeadersOnly,
		Raw:         a.Raw,
		Window:      threadWindowAgent,
	}, nil)
}

// threadWindowAgent is the agent face's window statement. It differs from the
// operator's in the half that matters here: this view covers the topics THIS
// task subscribes to, so a peer's half of the exchange is invisible and the
// chain renders as an orphan-rooted fragment. A reader who does not know that
// will read a truncated conversation as a viewer bug — the line exists to
// stop that reading.
const threadWindowAgent = "agent thread: shows what is still on the board on the topics THIS task subscribes to — a peer's half of an exchange may be invisible, so a fragment here is the view working, not a bug; a topic dies when its last subscriber task finishes, and holds at most the last 64 messages"

// listSubscribedTopics fetches the calling task's subscription pattern list.
// The patterns are concrete topic names (the seeded chat.<short-id> plus
// anything the task subscribed to); a wildcard pattern would under-report
// here, and ListRetained per concrete name is how the messages are collected.
func listSubscribedTopics(ctx context.Context, conn *Conn) ([]string, error) {
	reqID := rand.Uint32()
	respCh := make(chan agentboard.ListSubscriptionsResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_ListSubscriptionsResponse {
			r := msg.ListSubscriptionsResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})
	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListSubscriptions}
	msg.SetListSubscriptions(agentboard.ListSubscriptionsRequest{RequestId: reqID})
	if err := conn.SendRaw(msg); err != nil {
		return nil, err
	}
	select {
	case r := <-respCh:
		out := make([]string, 0, len(r.Subscriptions))
		for _, s := range r.Subscriptions {
			out = append(out, string(s.Pattern))
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// retainedMetas fetches one topic's retained ring as BoardMessages. The
// payloads are NOT carried by ListRetained — they are fetched per seq by the
// caller, through the same scoped read `agent read` uses. Retracted messages
// never appear here: agent-facing paths drop them the moment their author
// calls retract, so the agent face shows what the task can actually see.
func retainedMetas(ctx context.Context, conn *Conn, topic string) ([]cli.BoardMessage, map[uint64]int, error) {
	reqID := rand.Uint32()
	respCh := make(chan agentboard.ListRetainedResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_ListRetainedResponse {
			r := msg.ListRetainedResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})
	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListRetained}
	req := agentboard.ListRetainedRequest{RequestId: reqID}
	req.SetTopic([]byte(topic))
	msg.SetListRetained(req)
	if err := conn.SendRaw(msg); err != nil {
		return nil, nil, err
	}
	select {
	case r := <-respCh:
		switch r.Status {
		case agentboard.PurgeStatus_NotFound:
			// An absent topic is a normal answer (it may have died with its
			// last subscriber); it contributes no messages.
			return nil, nil, nil
		case agentboard.PurgeStatus_Ok:
			out := make([]cli.BoardMessage, 0, len(r.Metas))
			sizes := make(map[uint64]int, len(r.Metas))
			for _, m := range r.Metas {
				out = append(out, cli.BoardMessage{
					Seq:              m.Seq,
					InReplyTo:        m.InReplyTo,
					FromTaskHex:      hexTask(m.FromTask),
					FromHostname:     string(m.FromHostname),
					FromAgentProfile: string(m.FromAgentProfile),
					ReplyToTopic:     string(m.ReplyToTopic),
					ReceivedAtMs:     m.ReceivedAtUnixMs,
				})
				// The published size is known here and only here: ListRetained
				// carries metadata, not payloads. It rides separately so the
				// renderer can state it even when the body is never fetched
				// (--headers-only).
				sizes[m.Seq] = int(m.Size)
			}
			return out, sizes, nil
		default:
			return nil, nil, fmt.Errorf("retained %s: unexpected status %v", topic, r.Status)
		}
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// fetchBody reads one message's payload through the read path — scoped to
// the topics this task subscribes to, so it needs no capability either.
func fetchBody(ctx context.Context, conn *Conn, seq uint64) ([]byte, error) {
	reqID := rand.Uint32()
	respCh := make(chan agentboard.ReadSeqResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_ReadSeqResponse {
			r := msg.ReadSeqResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})
	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ReadSeq}
	if !msg.SetReadSeq(agentboard.ReadSeqRequest{RequestId: reqID, Seq: seq}) {
		return nil, errors.New("agent: SetReadSeq failed")
	}
	if err := conn.SendRaw(msg); err != nil {
		return nil, err
	}
	select {
	case r := <-respCh:
		if r.Status != agentboard.ReadSeqStatus_Ok || len(r.Msgs) == 0 {
			return nil, fmt.Errorf("seq %d: no readable body (it rotated out, or sits on a topic this task does not subscribe to)", seq)
		}
		payload, err := conn.FetchDeliveredPayload(ctx, r.Msgs[0].PayloadStreamId)
		if err != nil {
			return nil, fmt.Errorf("fetch payload seq=%d: %w", seq, err)
		}
		return payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// hexTask renders a TaskID the way BoardMessage exposes it.
func hexTask(t agentboard.TaskID) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(t.Id)*2)
	for _, b := range t.Id {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}
