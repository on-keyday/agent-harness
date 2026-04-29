package agent

import (
	"bytes"
	"context"
	"flag"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Inbox returns the JSON-Lines dump of pending messages on subscribed topics.
// This is the entry point invoked by the .claude/settings.json
// UserPromptSubmit hook. Output goes to stdout.
//
// `--since-last` alone is a peek: it reads from the prev-cursor snapshot
// (the position just before the most recent advance) so a manually
// invoked inbox returns the same batch the hook just delivered to the
// prompt context. The persisted cursor is NOT modified.
//
// `--since-last --commit` is the consuming form used by the hooks: it
// reads from the live cursor, advances it to NextCursor, and snapshots
// the old live position into prev-cursor. Calling --commit by hand will
// suppress the next hook's delivery of those seqs.
//
// With --stop-hook, the output instead becomes a single JSON object
// {"decision":"block","reason":<JSON-Lines>} that Claude Code's Stop hook
// uses to keep the agent looping when new agentboard messages arrive
// mid-turn. Empty inbox in --stop-hook mode produces no output.
func Inbox(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent inbox", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	taskID := fs.String("task-id", "", "")
	runnerID := fs.String("runner-id", "", "")
	sinceLast := fs.Bool("since-last", false, "use persisted cursor (peek; combine with --commit to advance)")
	commit := fs.Bool("commit", false, "advance the persisted cursor (hook use only — manual callers should leave this off)")
	since := fs.Uint64("since", 0, "cursor (ignored if --since-last)")
	asJSON := fs.Bool("json", false, "output JSON Lines (current default; flag accepted for forward compat)")
	stopHook := fs.Bool("stop-hook", false, "wrap output as Claude Code Stop-hook block decision")
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
	var oldLive uint64
	if *sinceLast {
		live, prev, err := LoadCursor(hexTaskID(conn.TaskID()))
		if err == nil {
			oldLive = live
			if *commit {
				cursor = live
			} else {
				cursor = prev
			}
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
		if *stopHook {
			var reason bytes.Buffer
			for _, m := range r.Msgs {
				emitMessageLine(&reason, m.Seq, string(m.Topic), m.Payload, m.FromRunnerId, m.FromTaskId, string(m.FromHostname))
			}
			emitStopHookOutput(stdout, reason.String())
		} else {
			for _, m := range r.Msgs {
				emitMessageLine(stdout, m.Seq, string(m.Topic), m.Payload, m.FromRunnerId, m.FromTaskId, string(m.FromHostname))
			}
		}
		if *sinceLast && *commit {
			_ = SaveCursor(hexTaskID(conn.TaskID()), r.NextCursor, oldLive)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
