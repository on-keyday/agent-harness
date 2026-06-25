package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
)

// Purge is the entry for `harness-cli agent purge`. It destroys a topic's
// retained-message ring on the server (Capability_Purge required), flushing an
// unwanted payload out of the server-side buffer so a since=0 re-read can't
// resurface it. The cursor stays valid: the board seq counter is global, so a
// post-purge message gets a strictly higher seq.
func Purge(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent purge", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	topic := fs.String("topic", "", "topic whose retained buffer to purge (exact match in v1)")
	self := fs.Bool("self", false, "purge this agent's own inbound topic (chat.<first-8-hex-of-task-id>); mutually exclusive with --topic")
	seq := fs.Uint64("seq", 0, "drop only the retained message with this seq (0 = the whole topic). Find seqs with `agent retained`.")
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
	respCh := make(chan agentboard.PurgeResponse, 1)
	conn.SetOnControl(func(k appwire.AppKind, p []byte) {
		if k != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_PurgeResponse {
			r := msg.PurgeResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Purge}
	req := agentboard.PurgeRequest{RequestId: reqID, Seq: *seq}
	req.SetTopic([]byte(*topic))
	msg.SetPurge(req)
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		switch r.Status {
		case agentboard.PurgeStatus_Ok:
			fmt.Fprintf(stdout, "{\"status\":\"ok\",\"topic\":%q,\"purged\":%d}\n", *topic, r.Purged)
			return nil
		case agentboard.PurgeStatus_NotFound:
			// Idempotent: nothing matched (topic never created / already evicted,
			// or --seq named a message no longer in the ring). Not an error.
			fmt.Fprintf(stdout, "{\"status\":\"not_found\",\"topic\":%q,\"purged\":0}\n", *topic)
			return nil
		case agentboard.PurgeStatus_Denied:
			return errors.New("purge denied: requires capability \"purge\"")
		default:
			return fmt.Errorf("purge: unexpected status %v", r.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}
