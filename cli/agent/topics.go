package agent

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
)

// Topics fetches the board-wide topic list and emits one JSON Lines record per topic.
func Topics(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent topics", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	taskID := fs.String("task-id", "", "")
	runnerID := fs.String("runner-id", "", "")
	if err := fs.Parse(args); err != nil {
		return err
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
	respCh := make(chan agentboard.ListTopicsResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_ListTopicsResponse {
			r := msg.ListTopicsResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListTopics}
	msg.SetListTopics(agentboard.ListTopicsRequest{RequestId: reqID})
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		for _, s := range r.Topics {
			rec := map[string]any{
				"name":              string(s.Name),
				"last_seq":          s.LastSeq,
				"last_published_at": time.UnixMilli(int64(s.LastPublishedAtUnixMs)).UTC().Format(time.RFC3339),
				"msg_count":         s.MsgCount,
			}
			line, _ := json.Marshal(rec)
			fmt.Fprintln(stdout, string(line))
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
