package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
)

// Retained is the entry for `harness-cli agent retained`. It lists a topic's
// retained ring as METADATA ONLY (seq / sender / size / time) — no payload
// bytes are returned. This is the content-blind way to pick which message to
// `agent purge --seq N` without ingesting a payload that might itself trip a
// moderation gate. No cap gate (like inbox/wait) — it is a keyed read of an
// already-named topic and surfaces a strict subset of what subscribing + inbox
// already returns uncapped. Output is JSON Lines, one object per retained message.
func Retained(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent retained", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	topic := fs.String("topic", "", "topic whose retained ring to list (exact match in v1)")
	self := fs.Bool("self", false, "list this agent's own inbound topic (chat.<first-8-hex-of-task-id>); mutually exclusive with --topic")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
			// No such topic (never created / evicted). Empty, not an error.
			return nil
		case agentboard.PurgeStatus_Ok:
			for _, m := range r.Metas {
				fmt.Fprintf(stdout,
					"{\"seq\":%d,\"from_task\":%q,\"from_hostname\":%q,\"size\":%d,\"received_at_ms\":%d}\n",
					m.Seq, hex.EncodeToString(m.FromTask.Id[:]), string(m.FromHostname), m.Size, m.ReceivedAtUnixMs)
			}
			return nil
		default:
			return fmt.Errorf("retained: unexpected status %v", r.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}
