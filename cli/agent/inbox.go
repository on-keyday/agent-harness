package agent

import (
	"context"
	"flag"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Inbox returns the JSON-Lines dump of pending messages on subscribed topics.
// This is the entry point invoked by the .claude/settings.json
// UserPromptSubmit hook. Output goes to stdout; cursor file is updated when
// --since-last is set.
func Inbox(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent inbox", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	taskID := fs.String("task-id", "", "")
	runnerID := fs.String("runner-id", "", "")
	sinceLast := fs.Bool("since-last", false, "use persisted cursor")
	since := fs.Uint64("since", 0, "cursor (ignored if --since-last)")
	asJSON := fs.Bool("json", false, "output JSON Lines (current default; flag accepted for forward compat)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = asJSON // currently always JSON Lines

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
		TaskID:    *taskID,
		RunnerID:  *runnerID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	cursor := *since
	if *sinceLast {
		c, err := LoadCursor(hexTaskID(conn.TaskID()))
		if err == nil {
			cursor = c
		}
	}

	reqID := rand.Uint32()
	respCh := make(chan agentboard.InboxResponse, 1)
	conn.SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
		if kind != wire.ApplicationPayloadKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_InboxResponse {
			r := msg.InboxResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Inbox}
	msg.SetInbox(agentboard.InboxRequest{RequestId: reqID, Since: cursor})
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		for _, m := range r.Msgs {
			emitMessageLine(stdout, m.Seq, string(m.Topic), m.Payload)
		}
		if *sinceLast {
			_ = SaveCursor(hexTaskID(conn.TaskID()), r.NextCursor)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
