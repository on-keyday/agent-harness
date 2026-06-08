package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// WatchNotifications subscribes to the notifications topic and writes one JSON
// object per line to out for each NotifyEvent (ring backlog first, then live).
// Method form: callable on an existing *Client (TUI/WebUI reuse their client).
func (c *Client) WatchNotifications(ctx context.Context, out io.Writer) error {
	topic := topics.Notifications()
	stream, err := c.Peer().JoinAndGetStream(ctx, "notify-watch", topic)
	if err != nil {
		return fmt.Errorf("join %s: %w", topic, err)
	}
	var mu sync.Mutex
	go func() {
		var buf []byte
		for {
			data, eof, rerr := stream.ReadDirect(4096)
			if rerr != nil {
				return
			}
			if len(data) > 0 {
				buf = append(buf, data...)
				buf = drainNotifyEvents(buf, out, &mu)
			}
			if eof {
				return
			}
		}
	}()
	<-ctx.Done()
	return ctx.Err()
}

// WatchNotifications (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).WatchNotifications instead.
func WatchNotifications(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.WatchNotifications(ctx, out)
}

// drainNotifyEvents decodes as many whole NotifyEvents as buf holds, writing one
// JSON line each, and returns the undrained remainder.
func drainNotifyEvents(buf []byte, out io.Writer, mu *sync.Mutex) []byte {
	for {
		ev := &protocol.NotifyEvent{}
		rest, err := ev.Decode(buf)
		if err != nil {
			break
		}
		mu.Lock()
		line, _ := json.Marshal(notifyEventJSON(ev))
		fmt.Fprintf(out, "%s\n", line)
		mu.Unlock()
		buf = rest
	}
	return buf
}

func notifyEventJSON(ev *protocol.NotifyEvent) map[string]any {
	m := map[string]any{
		"ts":     ev.Ts,
		"level":  ev.Level.String(),
		"origin": ev.Origin.String(),
		"title":  string(ev.Title),
		"text":   string(ev.Text),
	}
	if w := ev.Worker(); w != nil {
		m["task_id"] = string(w.TaskId)
		m["runner_id"] = string(w.RunnerId)
		m["repo"] = string(w.Repo)
		m["hostname"] = string(w.Hostname)
	}
	return m
}
