//go:build js

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
)

// What a page that was asleep needs from the persist loop (see
// webui/static/reconnect.js for the page's side).
//
//   - reconnectKick ends a backoff sleep at once and resets the attempt count
//     (cli.PersistConfig.Kick).
//   - The backoff is capped low while the page is visible, where an operator is
//     watching a "reconnecting" banner, and may grow while it is hidden, where
//     a retry every few seconds only spends battery.
//   - previewStart gives up waiting for an unanswering connection after
//     previewStartTimeout and says so, instead of leaving "connecting…" up.
const (
	visibleMaxBackoff   = 5 * time.Second
	hiddenMaxBackoff    = 30 * time.Second // = cli.PersistConfig's own default cap
	previewStartTimeout = 10 * time.Second
)

var (
	// Buffered 1 so a kick sent while the loop is between sleeps is not lost,
	// and a burst of wake events (visible + online + pageshow) is one kick.
	reconnectKick = make(chan struct{}, 1)
	pageVisible   atomic.Bool
)

func init() { pageVisible.Store(true) }

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

// probeLive asks the server for the caller's identity (whoami: no capability,
// one small datagram each way, already part of the wire) and reports whether
// an answer came within d. A connection the network dropped while the phone
// slept gives no close event, so only an unanswered request reveals it. An
// answer of any kind — even an error response — proves the link is alive; only
// the deadline expiring counts as dead.
func probeLive(c *cli.Client, d time.Duration) bool {
	ctx, cancel := context.WithTimeout(rootCtx, d)
	defer cancel()
	_, err := cli.WhoAmIWith(ctx, c)
	return !(err != nil && ctx.Err() != nil)
}

// harnessProbeConnection verifies the current connection answers within
// timeoutMs. On no answer it closes that connection — which makes the persist
// loop's watcher see Done and dial again — and kicks the loop so the dial is
// immediate and the attempt count restarts.
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
			c, err := currentClient()
			if err != nil {
				sendKick() // not connected: nothing to probe, just dial now
				resolve.Invoke(false)
				return
			}
			if probeLive(c, d) {
				resolve.Invoke(true)
				return
			}
			c.Close()
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
