package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// watchNotifications subscribes to the notifications topic and writes one
// formatted line per NotifyEvent to out (ring backlog first, then live), using
// the given per-event line formatter. Blocks until ctx is done.
func (c *Client) watchNotifications(ctx context.Context, out io.Writer, line func(*protocol.NotifyEvent) string) error {
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
				buf = drainNotifyEvents(buf, out, &mu, line)
			}
			if eof {
				return
			}
		}
	}()
	<-ctx.Done()
	return ctx.Err()
}

// WatchNotifications writes one JSON object per line (machine-readable). Used by
// the TUI/WebUI wasm consumers. Method form: callable on an existing *Client.
func (c *Client) WatchNotifications(ctx context.Context, out io.Writer) error {
	return c.watchNotifications(ctx, out, notifyEventJSONLine)
}

// WatchNotificationsText writes one human-readable line per event — for
// `harness-cli notify-watch`, mirroring cli.Watch's line output.
func (c *Client) WatchNotificationsText(ctx context.Context, out io.Writer) error {
	return c.watchNotifications(ctx, out, notifyEventTextLine)
}

// WatchNotifications (package-level) opens a fresh Client per call (short-lived
// harness-cli). Writes JSON lines. Long-lived consumers hold a *Client.
func WatchNotifications(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.WatchNotifications(ctx, out)
}

// WatchNotificationsText (package-level) is the human-readable variant for the
// `harness-cli notify-watch` subcommand.
func WatchNotificationsText(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.WatchNotificationsText(ctx, out)
}

// drainNotifyEvents decodes as many whole NotifyEvents as buf holds, writing one
// formatted line each, and returns the undrained remainder.
func drainNotifyEvents(buf []byte, out io.Writer, mu *sync.Mutex, line func(*protocol.NotifyEvent) string) []byte {
	for {
		ev := &protocol.NotifyEvent{}
		rest, err := ev.Decode(buf)
		if err != nil {
			break
		}
		mu.Lock()
		fmt.Fprintln(out, line(ev))
		mu.Unlock()
		buf = rest
	}
	return buf
}

func notifyEventJSONLine(ev *protocol.NotifyEvent) string {
	b, _ := json.Marshal(notifyEventJSON(ev))
	return string(b)
}

// notifyEventTextLine renders "15:04:05 [level] title — text  (origin[/host][ taskid])",
// matching the TUI pane's renderNotifyEvent.
func notifyEventTextLine(ev *protocol.NotifyEvent) string {
	ts := time.Unix(int64(ev.Ts), 0).Local().Format("15:04:05")
	origin := ev.Origin.String()
	if w := ev.Worker(); w != nil {
		if len(w.Hostname) > 0 {
			origin += "/" + string(w.Hostname)
		}
		if id := string(w.TaskId); len(id) > 0 {
			origin += " " + id
		}
	}
	body := string(ev.Title)
	if text := string(ev.Text); text != "" {
		if body != "" {
			body += " — " + text
		} else {
			body = text
		}
	}
	return fmt.Sprintf("%s [%s] %s  (%s)", ts, ev.Level.String(), body, origin)
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
