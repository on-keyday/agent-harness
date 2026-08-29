//go:build js

package cli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall/js"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// pinSlot is one rendered preview's REGISTERED REACH: the forward registration
// that says "this preview may reach host:port", held for as long as the preview
// exists rather than for the length of one request.
//
// Registering the pin instead of each fetch is what makes the reach visible.
// Per-fetch registrations lived tens of milliseconds against a registry with no
// history and a 5s poll, so nothing the page did ever appeared in `forward ls`
// — see docs/superpowers/specs/2026-08-12-preview-pin-forward-registration-design.md.
//
// Same shape and same reason as rawSlots in raw_forward_wasm.go: the register
// RPC is asynchronous, so a preview closed while it is in flight must discard
// the registration rather than install an orphan (stop-wins).
type pinSlot struct {
	ctrl      trsf.BidirectionalStream
	forwardID uint64
	taskID    string
	host      string
	port      int
	gen       uint64
	stop      context.CancelFunc
	// closedLocally records that WE closed the registration, so the control
	// watcher does not report our own teardown to the page as a revocation.
	// Same trap RawConn.closedLocally exists for.
	closedLocally atomic.Bool
	note          string
}

var (
	pinMu    sync.Mutex
	pinSlots = map[string]*pinSlot{}
	pinGen   atomic.Uint64
)

// OpenPreviewPin registers the reach for the preview under key and holds it.
// Any previous registration for key is superseded and closed first. Returns the
// forward id so the page can show what an operator would kill.
func OpenPreviewPin(ctx context.Context, c *Client, key, taskIDHex, host string, port int) (uint64, error) {
	pinMu.Lock()
	old := pinSlots[key]
	gen := pinGen.Add(1)
	pinSlots[key] = &pinSlot{gen: gen, taskID: taskIDHex, host: host, port: port}
	pinMu.Unlock()
	if old != nil {
		closePinSlot(old)
	}

	ctrl, fid, err := c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Local,
		"", 0, host, port, protocol.ClientEndpointKind_InProcessPreview)
	if err != nil {
		pinMu.Lock()
		if slot := pinSlots[key]; slot != nil && slot.gen == gen {
			delete(pinSlots, key)
		}
		pinMu.Unlock()
		return 0, err
	}

	pinMu.Lock()
	slot := pinSlots[key]
	if slot == nil || slot.gen != gen {
		pinMu.Unlock()
		_ = ctrl.CloseBoth() // superseded while registering: discard
		return 0, nil
	}
	watchCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	slot.ctrl = ctrl
	slot.forwardID = fid
	slot.stop = stop
	pinMu.Unlock()

	go func() {
		serveForwardControl(watchCtx, ctrl, func(line string) {
			pinMu.Lock()
			if s := pinSlots[key]; s != nil && s.gen == gen && !s.closedLocally.Load() {
				s.note = line
			}
			pinMu.Unlock()
		}, func() {
			// The registration ended: either an operator killed it from another
			// surface, or the server connection went. Either way the preview's
			// reach is gone, so the page must stop believing it has one.
			pinMu.Lock()
			s := pinSlots[key]
			mine := s != nil && s.gen == gen
			local := mine && s.closedLocally.Load()
			reason := "forward closed"
			if mine && s.note != "" {
				reason = s.note
			}
			if mine {
				delete(pinSlots, key)
			}
			pinMu.Unlock()
			if !mine || local {
				return // superseded, or our own teardown: not a revocation
			}
			if fn := js.Global().Get("harness_previewPinClosed"); fn.Type() == js.TypeFunction {
				fn.Invoke(key, reason)
			}
		})
	}()
	return fid, nil
}

// PreviewPinFetch sends one HTTP request over a connection opened under the
// pin. The connection itself is NOT registered: the pin already represents it,
// for longer and more visibly than a per-request row ever did.
func PreviewPinFetch(ctx context.Context, c *Client, key string, spec HTTPRequestSpec) (*HTTPFetchResult, error) {
	pinMu.Lock()
	slot := pinSlots[key]
	var taskID, host string
	var port int
	if slot != nil {
		taskID, host, port = slot.taskID, slot.host, slot.port
	}
	pinMu.Unlock()
	if slot == nil || slot.ctrl == nil {
		return nil, errors.New("previewPinFetch: no live pin for this preview")
	}
	req, method, err := buildFetchRequest(spec, host, port)
	if err != nil {
		return nil, err
	}
	st, err := c.OpenPortForward(ctx, taskID, host, port)
	if err != nil {
		return nil, err
	}
	defer st.CloseBoth()
	// Not half-closed after the request: the runner splices with
	// either-side-wins teardown, so a half-close propagates as a full close and
	// discards the reply (see spliceStdio's comment in forward_stdio.go).
	// Connection: close on the request is what ends it instead.
	chunk := make([]byte, len(req))
	copy(chunk, req)
	if err := st.AppendData(false, chunk); err != nil {
		return nil, err
	}
	return readFetchResponse(method, func() ([]byte, bool, error) {
		return st.ReadDirectContext(ctx, 64*1024)
	})
}

// ClosePreviewPin drops the registration for key. The generation bump silences
// the watcher, so the page sees no harness_previewPinClosed for a close it
// asked for. Idempotent, and safe while OpenPreviewPin is still registering.
func ClosePreviewPin(key string) {
	pinMu.Lock()
	slot := pinSlots[key]
	delete(pinSlots, key)
	pinGen.Add(1)
	pinMu.Unlock()
	closePinSlot(slot)
}

func closePinSlot(slot *pinSlot) {
	if slot == nil {
		return
	}
	slot.closedLocally.Store(true)
	if slot.stop != nil {
		slot.stop()
	}
	if slot.ctrl != nil {
		_ = slot.ctrl.CloseBoth()
	}
}
