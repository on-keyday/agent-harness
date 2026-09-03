package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// Retained is the entry for `harness-cli agent retained`. It lists a topic's
// retained ring as METADATA ONLY (seq / sender / size / time) — no payload
// bytes are returned. This is the content-blind way to pick which message to
// `agent purge --seq N` without ingesting a payload that might itself trip a
// moderation gate. No cap gate (like inbox/wait) — it is a keyed read of an
// already-named topic and surfaces a strict subset of what subscribing + inbox
// already returns uncapped. Output is JSON Lines, one object per retained message.
func Retained(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("retained", args)
	if perr != nil {
		return perr
	}
	return RetainedWith(ctx, a, stdout)
}

// RetainedWith is Retained for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func RetainedWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID
	topic := &a.Topic
	self := &a.Self
	if *self && *topic != "" {
		return errors.New("--self and --topic are mutually exclusive")
	}
	if *self {
		tid, err := cliopts.ResolveTaskID("")
		if err != nil {
			return err
		}
		t := SelfTopic(tid)
		topic = &t
	}
	if *topic == "" {
		return errors.New("--topic or --self required")
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.ListRetainedResponse, 1)
	conn.SetOnControl(func(k appwire.AppKind, p []byte) {
		if k != appwire.AppKind_AgentMessage {
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
	req.SetTopic([]byte(*topic))
	msg.SetListRetained(req)
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		switch r.Status {
		case agentboard.PurgeStatus_NotFound:
			// Exit 0 — an absent topic is a normal answer, not an error. But
			// say WHICH nothing this is: "no such topic" and "topic with an
			// empty ring" both print nothing on stdout and share an exit code,
			// so a typo'd topic name is indistinguishable from a quiet one.
			// The wire already separates them (Status); only this rendering
			// collapsed them. Diagnostics go to stderr so `agent retained
			// --topic T | jq` stays a clean JSON-Lines stream — the same split
			// cli/cmd_board.go uses for `board read`.
			fmt.Fprintf(os.Stderr, "agent retained: topic %q is not on the board (never published, or evicted / purged)\n", *topic)
			return nil
		case agentboard.PurgeStatus_Ok:
			if len(r.Metas) == 0 {
				fmt.Fprintf(os.Stderr, "agent retained: topic %q is on the board but holds no messages\n", *topic)
				return nil
			}
			for _, m := range r.Metas {
				// Marshalled rather than Fprintf'd: %q renders a GO string
				// literal, which is not JSON for every input, and this line
				// gained an optional field. A struct keeps the field ORDER the
				// documented sample shows, which a map would sort away.
				line, _ := json.Marshal(retainedLine{
					Seq:          m.Seq,
					InReplyTo:    m.InReplyTo,
					FromTask:     hex.EncodeToString(m.FromTask.Id[:]),
					FromHostname: string(m.FromHostname),
					FromAgent:    string(m.FromAgentProfile),
					ReplyToTopic: string(m.ReplyToTopic),
					Size:         m.Size,
					ReceivedAtMs: m.ReceivedAtUnixMs,
				})
				fmt.Fprintln(stdout, string(line))
			}
			return nil
		default:
			return fmt.Errorf("retained: unexpected status %v", r.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retainedLine is one `agent retained` output record. reply_to_topic is
// omitempty because a sender that declared no destination is the ordinary
// case: printing "" on every row would spend a field per message on saying
// nothing was declared.
type retainedLine struct {
	Seq          uint64 `json:"seq"`
	InReplyTo    uint64 `json:"in_reply_to"`
	FromTask     string `json:"from_task"`
	FromHostname string `json:"from_hostname"`
	FromAgent    string `json:"from_agent"`
	ReplyToTopic string `json:"reply_to_topic,omitempty"`
	Size         uint32 `json:"size"`
	ReceivedAtMs uint64 `json:"received_at_ms"`
}
