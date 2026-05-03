package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func subscribeOrUnsub(ctx context.Context, args []string, stdout io.Writer, kind agentboard.AgentMessageKind) error {
	fs := flag.NewFlagSet("agent subscribe", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	taskID := fs.String("task-id", "", "")
	runnerID := fs.String("runner-id", "", "")
	pattern := fs.String("topic", "", "topic to subscribe (exact match in v1)")
	self := fs.Bool("self", false, "subscribe to this agent's inbound topic (chat.<first-8-hex-of-task-id>); mutually exclusive with --topic")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *self && *pattern != "" {
		return errors.New("--self and --topic are mutually exclusive")
	}
	if *self {
		tid, err := cliopts.ResolveTaskID(*taskID)
		if err != nil {
			return err
		}
		t := SelfTopic(tid)
		pattern = &t
	}
	if *pattern == "" {
		return errors.New("--topic or --self required")
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
		TaskID:    *taskID,
		RunnerID:  *runnerID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.SubscribeResponse, 1)
	conn.SetOnControl(func(k wire.ApplicationPayloadKind, p []byte) {
		if k != wire.ApplicationPayloadKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_SubscribeResponse {
			r := msg.SubscribeResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: kind}
	if kind == agentboard.AgentMessageKind_Subscribe {
		req := agentboard.SubscribeRequest{RequestId: reqID}
		req.SetPattern([]byte(*pattern))
		msg.SetSubscribe(req)
	} else {
		req := agentboard.UnsubscribeRequest{RequestId: reqID}
		req.SetPattern([]byte(*pattern))
		msg.SetUnsubscribe(req)
	}
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		if r.Status != agentboard.SubscribeStatus_Ok {
			return fmt.Errorf("subscribe failed: %v", r.Status)
		}
		fmt.Fprintln(stdout, `{"status":"ok"}`)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe is the entry for `harness-cli agent subscribe`.
func Subscribe(ctx context.Context, args []string, stdout io.Writer) error {
	return subscribeOrUnsub(ctx, args, stdout, agentboard.AgentMessageKind_Subscribe)
}

// Unsubscribe is the entry for `harness-cli agent unsubscribe`.
func Unsubscribe(ctx context.Context, args []string, stdout io.Writer) error {
	return subscribeOrUnsub(ctx, args, stdout, agentboard.AgentMessageKind_Unsubscribe)
}
