//go:build js

package cli

import (
	"context"
	"errors"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"sync"
	"sync/atomic"
	"syscall/js"
)

// rawSlot is one pane's raw connection plus a generation guard. Same shape and
// same reason as previewSlots in preview_wasm.go: the open is asynchronous, so a
// pane closed while OpenRawForward is still in flight must discard the
// connection rather than install an orphan (stop-wins).
type rawSlot struct {
	conn *RawConn
	gen  uint64
	// host/port are the forward's target, kept because BuildHTTPRequest needs
	// them for the Host header — the page must never assemble request bytes
	// itself, so it cannot supply them either.
	host string
	port int
	// note holds the last line the control stream produced — why a remote kill
	// happened, or that the server connection was lost — so the single terminal
	// harness_rawClosed can carry it. The control watcher must not fire its own
	// close hook: it and the pump are two paths for one event.
	note string
}

var (
	rawMu    sync.Mutex
	rawSlots = map[string]*rawSlot{}
	rawGen   atomic.Uint64 // monotonic across ALL panes; every open/close reserves one
)

// OpenRawPane opens a raw forward for the pane JS has already created under
// paneKey. Any previous connection for paneKey is superseded and closed first.
// Returns nil (not an error) when a close or a newer open superseded this one
// while it was connecting — the superseding caller owns the pane's UI state.
func OpenRawPane(ctx context.Context, c *Client, paneKey, taskIDHex, host string, port int) error {
	rawMu.Lock()
	old := rawSlots[paneKey]
	gen := rawGen.Add(1)
	rawSlots[paneKey] = &rawSlot{gen: gen, host: host, port: port}
	rawMu.Unlock()
	if old != nil && old.conn != nil {
		_ = old.conn.Close()
	}

	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, protocol.ClientEndpointKind_InProcessPane, func(line string) {
		rawMu.Lock()
		if slot := rawSlots[paneKey]; slot != nil && slot.gen == gen {
			slot.note = line
		}
		rawMu.Unlock()
	})
	if err != nil {
		rawMu.Lock()
		if slot := rawSlots[paneKey]; slot != nil && slot.gen == gen {
			delete(rawSlots, paneKey)
		}
		rawMu.Unlock()
		return err
	}

	rawMu.Lock()
	slot := rawSlots[paneKey]
	if slot == nil || slot.gen != gen {
		rawMu.Unlock()
		_ = rc.Close() // superseded while connecting: discard, do not install
		return nil
	}
	slot.conn = rc
	rawMu.Unlock()

	go rawPump(paneKey, rc, gen)
	return nil
}

// SendRawPaneHTTP builds a request for the pane's own target and writes it in
// one call. Same builder as the CLI and the TUI, so a request that works from
// one surface works from all three.
//
// Returns how many bytes went out: the page cannot count them itself — it
// deliberately never assembles the request — and without the count its "out"
// counter sat at 0B after every HTTP send.
func SendRawPaneHTTP(key string, spec HTTPRequestSpec) (int, error) {
	rawMu.Lock()
	slot := rawSlots[key]
	rawMu.Unlock()
	if slot == nil || slot.conn == nil {
		return 0, errors.New("rawSendHTTP: no such pane")
	}
	req, err := BuildHTTPRequest(spec, slot.host, slot.port)
	if err != nil {
		return 0, err
	}
	if err := slot.conn.Send(req); err != nil {
		return 0, err
	}
	return len(req), nil
}

// SendRawPane writes bytes to the pane's connection.
func SendRawPane(key string, data []byte) error {
	rawMu.Lock()
	slot := rawSlots[key]
	var rc *RawConn
	if slot != nil {
		rc = slot.conn
	}
	rawMu.Unlock()
	if rc == nil {
		return errors.New("rawSend: no such pane")
	}
	return rc.Send(data)
}

// CloseRawPane closes the pane's connection, deregistering the forward. The
// generation bump silences the pump's remaining callbacks, so JS sees no
// harness_rawClosed for a close it initiated itself. Idempotent, and safe to
// call while OpenRawPane is still connecting — that is what supersedes it.
func CloseRawPane(key string) {
	rawMu.Lock()
	slot := rawSlots[key]
	delete(rawSlots, key)
	rawGen.Add(1)
	rawMu.Unlock()
	if slot != nil && slot.conn != nil {
		_ = slot.conn.Close()
	}
}

// rawPump forwards received bytes to the page until the connection ends, then
// closes it and fires the one terminal notification.
func rawPump(paneKey string, rc *RawConn, gen uint64) {
	defer func() {
		// Closing deregisters the forward server-side; without it an ended
		// connection leaves a permanent row in `forward ls`.
		_ = rc.Close()

		rawMu.Lock()
		slot := rawSlots[paneKey]
		mine := slot != nil && slot.gen == gen
		reason := "connection closed"
		if mine && slot.note != "" {
			reason = slot.note
		}
		rawMu.Unlock()
		if !mine {
			return // superseded: the superseding caller owns the pane's UI state
		}
		// Fire before deleting: rawCall's staleness check needs the slot.
		rawCall(paneKey, gen, "harness_rawClosed", reason)
		rawMu.Lock()
		if slot := rawSlots[paneKey]; slot != nil && slot.gen == gen {
			delete(rawSlots, paneKey)
		}
		rawMu.Unlock()
	}()

	for {
		data, eof, err := rc.Recv(context.Background())
		if len(data) > 0 {
			arr := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(arr, data)
			if !rawCall(paneKey, gen, "harness_rawData", arr) {
				return
			}
		}
		if eof || err != nil {
			return
		}
	}
}

// rawCall invokes the named JS hook with paneKey as its first argument, iff gen
// is still the pane's current generation; returns false when superseded so the
// pump exits silently. A missing hook (non-WebUI wasm host) is a no-op that
// keeps the pump alive.
func rawCall(key string, gen uint64, hook string, args ...any) bool {
	rawMu.Lock()
	slot := rawSlots[key]
	stale := slot == nil || slot.gen != gen
	rawMu.Unlock()
	if stale {
		return false
	}
	fn := js.Global().Get(hook)
	if fn.Type() != js.TypeFunction {
		return true
	}
	all := append([]any{key}, args...)
	fn.Invoke(all...)
	return true
}
