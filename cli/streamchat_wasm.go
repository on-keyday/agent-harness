//go:build js

package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall/js"

	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// The WebUI's half of the event-stream chat: one cowrite attach whose lines are
// pushed to JS hooks, and whose writes go back out over the same attach.
//
// Modelled on preview_wasm.go and deliberately narrower in one way: there is
// exactly ONE chat, not N panes. A pane key would be a parameter with a single
// value at every call site. The generation guard is kept in full, because its
// job is not multiplicity — it is stop-wins across an in-flight attach, which
// applies just as much to one chat as to nine panes.
//
// Reading and writing share the attach for the reason the TUI's does:
// reattaching per keystroke would re-replay the ring every time.
var (
	chatMu   sync.Mutex
	chatSlot *streamChatSlot
	chatGen  atomic.Uint64
)

type streamChatSlot struct {
	taskID string
	sess   *StreamSession
	gen    uint64
}

// StartStreamChat cowrite-attaches an event-stream task and pumps its NDJSON
// lines to the JS hooks. Any previous chat is superseded and closed first.
//
// The RAW line goes to JS, which parses it: this is the adapter protocol's own
// JSON, so JSON.parse is reading the grammar, not a second implementation of
// it. WRITING is the direction that must not be re-implemented, and it is not —
// every write goes through EncodeStreamMsg over the bridge below.
func (c *Client) StartStreamChat(ctx context.Context, taskIDHex string) error {
	chatMu.Lock()
	old := chatSlot
	gen := chatGen.Add(1)
	chatSlot = &streamChatSlot{taskID: taskIDHex, gen: gen}
	chatMu.Unlock()
	if old != nil && old.sess != nil {
		_ = old.sess.Close()
	}

	sess, err := c.OpenStreamSession(ctx, taskIDHex)
	if err != nil {
		// Clear the slot we reserved, or a failed attach leaves the UI looking
		// attached to a session that never opened.
		chatMu.Lock()
		if chatSlot != nil && chatSlot.gen == gen {
			chatSlot = nil
		}
		chatMu.Unlock()
		return err
	}

	chatMu.Lock()
	slot := chatSlot
	if slot == nil || slot.gen != gen {
		chatMu.Unlock()
		_ = sess.Close()
		return nil // superseded while attaching; whoever superseded us owns the UI
	}
	slot.sess = sess
	chatMu.Unlock()

	// The agent's stderr rides its own frame type and is not NDJSON. An
	// undrained side backpressures the whole stream, so drain it even though
	// this view does not show it — the task log carries it tagged [err].
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr()) }()
	go streamChatPump(taskIDHex, sess, gen)
	return nil
}

// StopStreamChat closes the chat's stream, if any. Idempotent. The generation
// bump silences the pump immediately, so JS sees no harness_streamClosed for a
// stop it initiated itself.
func StopStreamChat() {
	chatMu.Lock()
	old := chatSlot
	chatSlot = nil
	chatGen.Add(1)
	chatMu.Unlock()
	if old != nil && old.sess != nil {
		_ = old.sess.Close()
	}
}

// SendStreamChat writes one message on the held chat attach.
//
// taskIDHex is checked rather than trusted: a UI still holding an older task's
// id — a sheet left open across a chat switch — would otherwise send that
// task's turn into whatever chat is open now.
func SendStreamChat(taskIDHex string, m streamagent.Msg) error {
	chatMu.Lock()
	slot := chatSlot
	chatMu.Unlock()
	if slot == nil || slot.sess == nil {
		return fmt.Errorf("no chat attached")
	}
	if slot.taskID != taskIDHex {
		return fmt.Errorf("the chat is attached to %s, not %s", slot.taskID, taskIDHex)
	}
	return slot.sess.Send(m)
}

func streamChatPump(taskID string, sess *StreamSession, gen uint64) {
	defer sess.Close()
	if !chatCall(gen, "harness_streamOpen", taskID) {
		return
	}
	for {
		line, err := sess.ReadLine()
		if len(line.Raw) > 0 {
			if !chatCall(gen, "harness_streamLine", taskID, string(line.Raw)) {
				return
			}
		}
		if err != nil {
			msg := ""
			if err != io.EOF {
				msg = err.Error()
			}
			chatCall(gen, "harness_streamClosed", taskID, msg)
			return
		}
	}
}

// chatCall invokes the named JS hook iff gen is still current; returns false
// when superseded so the pump exits silently. A missing hook (a non-WebUI wasm
// host) is a no-op that keeps the pump alive.
func chatCall(gen uint64, fn string, args ...any) bool {
	chatMu.Lock()
	live := chatSlot != nil && chatSlot.gen == gen
	chatMu.Unlock()
	if !live {
		return false
	}
	f := js.Global().Get(fn)
	if f.Type() == js.TypeFunction {
		f.Invoke(args...)
	}
	return true
}
