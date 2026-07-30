//go:build js

package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall/js"
)

// rawSlot is one pane's raw connection plus a generation guard. Same shape and
// same reason as previewSlots in preview_wasm.go: the open is asynchronous, so a
// pane closed while OpenRawForward is still in flight must discard the
// connection instead of installing an orphan (stop-wins).
type rawSlot struct {
	conn *RawConn
	gen  uint64
}

var (
	rawMu     sync.Mutex
	rawSlots  = map[string]*rawSlot{}
	rawGen    atomic.Uint64 // monotonic across ALL panes; every open/close reserves one
	rawKeySeq atomic.Uint64
)

// OpenRawPane opens a raw forward for a new pane and starts pumping its bytes to
// the JS hooks. The returned key identifies the pane in every later call.
func OpenRawPane(ctx context.Context, c *Client, taskIDHex, host string, port int) (string, error) {
	key := fmt.Sprintf("raw%d", rawKeySeq.Add(1))

	rawMu.Lock()
	gen := rawGen.Add(1)
	rawSlots[key] = &rawSlot{gen: gen}
	rawMu.Unlock()

	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, func(line string) {
		rawCall(key, gen, "harness_rawClosed", line)
	})
	if err != nil {
		rawMu.Lock()
		if slot := rawSlots[key]; slot != nil && slot.gen == gen {
			delete(rawSlots, key)
		}
		rawMu.Unlock()
		return "", err
	}

	rawMu.Lock()
	slot := rawSlots[key]
	if slot == nil || slot.gen != gen {
		// Superseded (pane closed) while opening: discard rather than install.
		rawMu.Unlock()
		_ = rc.Close()
		return "", errors.New("rawOpen: pane closed while connecting")
	}
	slot.conn = rc
	rawMu.Unlock()

	go rawPump(key, rc, gen)
	return key, nil
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
// harness_rawClosed for a close it initiated itself. Idempotent.
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

// rawPump forwards received bytes to the page until the connection ends.
func rawPump(key string, rc *RawConn, gen uint64) {
	for {
		data, eof, err := rc.Recv(context.Background())
		if len(data) > 0 {
			arr := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(arr, data)
			if !rawCall(key, gen, "harness_rawData", arr) {
				return
			}
		}
		if eof || err != nil {
			rawCall(key, gen, "harness_rawClosed", "connection closed")
			rawMu.Lock()
			if slot := rawSlots[key]; slot != nil && slot.gen == gen {
				delete(rawSlots, key)
			}
			rawMu.Unlock()
			return
		}
	}
}

// rawCall invokes the named JS hook with key as its first argument, iff gen is
// still the pane's current generation; returns false when superseded so the pump
// exits silently. A missing hook (non-WebUI wasm host) is a no-op.
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
