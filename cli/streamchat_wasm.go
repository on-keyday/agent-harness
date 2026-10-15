//go:build js

package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"syscall/js"

	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// The WebUI's half of driving an event-stream session: cowrite attaches whose
// lines are pushed to JS hooks, and whose writes go back out over the same
// attach.
//
// Keyed like preview_wasm.go's panes: the chat holds the key "chat", a grid
// cell "grid:<taskId>". The two kinds of slot differ only in the hooks they
// call — the chat's hooks predate the keys and take the TASK id, a cell's take
// its key — and in the replay cap a cell asks for. The generation guard is
// per attach: it is stop-wins across an in-flight attach.
//
// Reading and writing share the attach for the reason the TUI's does:
// reattaching per keystroke would re-replay the ring every time.
var (
	streamMu    sync.Mutex
	streamSlots = map[string]*streamSlot{}
	streamGen   uint64
)

const streamChatKey = "chat"

type streamSlot struct {
	key    string
	taskID string
	sess   *StreamSession
	gen    uint64
	// hooks are the JS globals this slot reports to, and ident the first
	// argument each is called with.
	hooks streamHooks
	ident string
}

type streamHooks struct{ open, line, closed string }

var (
	chatHooks = streamHooks{"harness_streamOpen", "harness_streamLine", "harness_streamClosed"}
	paneHooks = streamHooks{"harness_streamPaneOpen", "harness_streamPaneLine", "harness_streamPaneClosed"}
)

// StartStreamChat cowrite-attaches an event-stream task for the chat and pumps
// its NDJSON lines to the chat's hooks. Any previous chat is superseded and
// closed first.
//
// The RAW line goes to JS, which parses it: this is the adapter protocol's own
// JSON, so JSON.parse is reading the grammar, not a second implementation of
// it. WRITING is the direction that must not be re-implemented, and it is not —
// every write goes through EncodeStreamMsg over the bridge below.
func (c *Client) StartStreamChat(ctx context.Context, taskIDHex string) error {
	return c.startStreamSlot(ctx, streamChatKey, taskIDHex, 0, chatHooks, taskIDHex)
}

// StartStreamPane is StartStreamChat for a grid cell: keyed, reporting to the
// pane hooks with the key, and with the ring replay capped like a PTY cell's —
// a cell shows the tail and would never render the rest.
func (c *Client) StartStreamPane(ctx context.Context, key, taskIDHex string) error {
	if key == streamChatKey {
		return fmt.Errorf("stream pane: %q is the chat's key", key)
	}
	return c.startStreamSlot(ctx, key, taskIDHex, GridPaneReplayLimit, paneHooks, key)
}

func (c *Client) startStreamSlot(ctx context.Context, key, taskIDHex string, replayLimit uint32, hooks streamHooks, ident string) error {
	streamMu.Lock()
	old := streamSlots[key]
	streamGen++
	gen := streamGen
	streamSlots[key] = &streamSlot{key: key, taskID: taskIDHex, gen: gen, hooks: hooks, ident: ident}
	streamMu.Unlock()
	if old != nil && old.sess != nil {
		_ = old.sess.Close()
	}

	sess, err := c.OpenStreamSessionWithReplayLimit(ctx, taskIDHex, replayLimit)
	if err != nil {
		// Clear the slot we reserved, or a failed attach leaves the UI looking
		// attached to a session that never opened.
		streamMu.Lock()
		if s := streamSlots[key]; s != nil && s.gen == gen {
			delete(streamSlots, key)
		}
		streamMu.Unlock()
		return err
	}

	streamMu.Lock()
	slot := streamSlots[key]
	if slot == nil || slot.gen != gen {
		streamMu.Unlock()
		_ = sess.Close()
		return nil // superseded while attaching; whoever superseded us owns the UI
	}
	slot.sess = sess
	streamMu.Unlock()

	// The agent's stderr rides its own frame type and is not NDJSON. It is
	// interleaved into the transcript, as in the TUI chat: this kind writes no
	// task log, so the chat is where an agent's complaint is seen. Draining it
	// is also what keeps it from backpressuring the whole stream.
	go streamStderrPump(slot, sess, gen)
	go streamPump(slot, sess, gen)
	return nil
}

// StopStreamChat closes the chat's stream, if any. Idempotent.
func StopStreamChat() { StopStreamSlot(streamChatKey) }

// StopStreamSlot closes one slot's stream, if any. Idempotent. Removing the
// slot silences its pump immediately, so JS sees no closed hook for a stop it
// initiated itself.
func StopStreamSlot(key string) {
	streamMu.Lock()
	old := streamSlots[key]
	delete(streamSlots, key)
	streamMu.Unlock()
	if old != nil && old.sess != nil {
		_ = old.sess.Close()
	}
}

// SendStreamChat writes one message on an attach to taskIDHex — the chat's if
// it holds that task, otherwise a grid cell's. Every slot is a cowrite attach
// on the task, so which one carries the line does not matter.
//
// taskIDHex is checked rather than trusted: a UI still holding an older task's
// id — a sheet left open across a chat switch — must not send that task's turn
// into whatever session is open now. A task no slot is attached to is refused.
func SendStreamChat(taskIDHex string, m streamagent.Msg) error {
	streamMu.Lock()
	var sess *StreamSession
	if s := streamSlots[streamChatKey]; s != nil && s.sess != nil && s.taskID == taskIDHex {
		sess = s.sess
	} else {
		for _, s := range streamSlots {
			if s.sess != nil && s.taskID == taskIDHex {
				sess = s.sess
				break
			}
		}
	}
	streamMu.Unlock()
	if sess == nil {
		return fmt.Errorf("no stream attached to %s", taskIDHex)
	}
	return sess.Send(m)
}

func streamPump(slot *streamSlot, sess *StreamSession, gen uint64) {
	defer sess.Close()
	if !streamCall(slot, gen, slot.hooks.open) {
		return
	}
	for {
		line, err := sess.ReadLine()
		if len(line.Raw) > 0 {
			if !streamCall(slot, gen, slot.hooks.line, string(line.Raw), displayForJS(line)) {
				return
			}
		}
		if err != nil {
			msg := ""
			if err != io.EOF {
				msg = err.Error()
			}
			streamCall(slot, gen, slot.hooks.closed, msg)
			return
		}
	}
}

// streamStderrPump hands each stderr line to the page as a line with its
// display decided, the same call a protocol line takes.
func streamStderrPump(slot *streamSlot, sess *StreamSession, gen uint64) {
	for {
		line, err := sess.ReadStderrLine()
		if len(line.Raw) > 0 {
			if !streamCall(slot, gen, slot.hooks.line, string(line.Raw), displayForJS(line)) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// displayForJS is streamagent.DisplayOf for the browser: the line, its tone
// (the page maps it to a `c-<tone>` class) and what it does to the status line,
// decided in Go so the WebUI chat and the TUI chat cannot classify one message
// two ways. A line that is not the protocol displays escaped, like everywhere
// else. null only for a decoded request, which the page renders from the raw
// line's payload into its approval control.
func displayForJS(line StreamLine) any {
	d, ok := LineDisplay(line)
	if !ok {
		return nil
	}
	return map[string]any{
		"text":      d.Text,
		"tone":      string(d.Tone),
		"setStatus": d.SetStatus,
		"status":    d.Status,
		"idle":      d.Idle,
		"resolves":  d.Resolves,
	}
}

// streamCall invokes the named JS hook with the slot's ident first, iff the
// attach is still its key's current one; returns false when superseded so the
// pump exits silently. A missing hook (a non-WebUI wasm host) is a no-op that
// keeps the pump alive.
func streamCall(slot *streamSlot, gen uint64, fn string, args ...any) bool {
	streamMu.Lock()
	cur := streamSlots[slot.key]
	live := cur != nil && cur.gen == gen
	streamMu.Unlock()
	if !live {
		return false
	}
	f := js.Global().Get(fn)
	if f.Type() == js.TypeFunction {
		f.Invoke(append([]any{slot.ident}, args...)...)
	}
	return true
}
