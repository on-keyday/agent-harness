//go:build js

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

// What a page that was asleep needs from the persist loop (see
// webui/static/reconnect.js for the page's side).
//
//   - reconnectKick ends a backoff sleep at once and resets the attempt count
//     (connauth.PersistConfig.Kick).
//   - The backoff is capped low while the page is visible, where an operator is
//     watching a "reconnecting" banner, and may grow while it is hidden, where
//     a retry every few seconds only spends battery.
//   - previewStart gives up waiting for an unanswering connection after
//     previewStartTimeout and says so, instead of leaving "connecting…" up.
const (
	visibleMaxBackoff   = 5 * time.Second
	hiddenMaxBackoff    = 30 * time.Second // = connauth.PersistConfig's own default cap
	previewStartTimeout = 10 * time.Second
)

var (
	// Buffered 1 so a kick sent while the loop is between sleeps is not lost,
	// and a burst of wake events (visible + online + pageshow) is one kick.
	reconnectKick = make(chan struct{}, 1)
	pageVisible   atomic.Bool
)

func init() { pageVisible.Store(true) }

// underlayDropper names the page's one endpoint and the server address its
// WebSocket goes to, so a probe that finds the link dead can drop that socket.
type underlayDropper struct {
	ep   objproto.Endpoint
	addr netip.AddrPort
}

// dropUnderlay is set once the page has built its endpoint.
var dropUnderlay atomic.Pointer[underlayDropper]

func sendKick() {
	select {
	case reconnectKick <- struct{}{}:
	default:
	}
}

func persistMaxBackoff() time.Duration {
	if pageVisible.Load() {
		return visibleMaxBackoff
	}
	return hiddenMaxBackoff
}

// harnessReconnectNow ends the persist loop's backoff sleep, if it is in one.
//
//	harness.reconnectNow()
func harnessReconnectNow(this js.Value, args []js.Value) any {
	sendKick()
	return js.Undefined()
}

// harnessSetPageVisible tells the bridge whether the page is visible; it picks
// the backoff cap for the NEXT sleep.
//
//	harness.setPageVisible(bool)
func harnessSetPageVisible(this js.Value, args []js.Value) any {
	if len(args) >= 1 {
		pageVisible.Store(args[0].Truthy())
	}
	return js.Undefined()
}

// harnessProbeConnection verifies the current connection answers within
// timeoutMs (cli.ProbeLive: whoami, no capability, one small datagram each way,
// already part of the wire). A connection the network dropped while the phone
// slept gives no close event, so only an unanswered request reveals it. The
// wait for a client is inside the same deadline: right after a reconnect the
// client is installed a moment after 'connected'. On no answer the probed
// connection is closed — which makes the persist loop's watcher see Done and
// dial again — and the loop is kicked so the dial is immediate and the attempt
// count restarts.
//
//	harness.probeConnection(timeoutMs) -> Promise<bool>   (false = declared dead)
func harnessProbeConnection(this js.Value, args []js.Value) any {
	d := 3 * time.Second
	if len(args) >= 1 && args[0].Type() == js.TypeNumber && args[0].Float() > 0 {
		d = time.Duration(args[0].Float() * float64(time.Millisecond))
	}
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		go func() {
			alive, c := cli.ProbeLive(rootCtx, d, func(ctx context.Context) (cli.TaskControlClient, error) {
				cl, err := waitForClient(ctx)
				if err != nil {
					return nil, err // a nil *Client must not become a non-nil interface
				}
				return cl, nil
			})
			if alive {
				resolve.Invoke(true)
				return
			}
			// Closing the connection alone is not enough after sleep: the
			// browser still reports the WebSocket OPEN, and the next handshake
			// would ride that dead socket. Drop it so the redial opens a new one.
			if d := dropUnderlay.Load(); d != nil {
				transport.CloseUnderlayAt(d.ep, d.addr, slog.Default())
			}
			if cl, ok := c.(*cli.Client); ok {
				cl.Close()
			}
			sendKick()
			resolve.Invoke(false)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// errPreviewTimeout is rejected to JS with code "timeout".
var errPreviewTimeout = errors.New("preview: no answer from the connection")

// rejectCoded rejects with an Error carrying .code, so the page can tell a
// timeout from a failure without matching message text.
func rejectCoded(reject js.Value, err error, code string) {
	e := js.Global().Get("Error").New(err.Error())
	e.Set("code", code)
	reject.Invoke(e)
}
