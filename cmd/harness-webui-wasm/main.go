//go:build js

// Command harness-webui-wasm is the wasm entry binary for the browser web UI.
// It exposes a Promise-based JS API on window.harness so the page-side
// JavaScript can drive the harness CLI flows (connect / submit / list /
// cancel / watch / prune / interactive*) without bundling a transport-aware
// JS client. See docs/superpowers/specs/2026-04-26-wasm-transport-design.md.
//
// The wasm side reuses the same cli.* helpers as the native CLI; the
// transport.WebSocketEndpoint chooses the wasm-specific implementation via
// build tags (transport/websocket_wasm.go). This file is the only piece
// that is wasm-only by build tag.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

var (
	rootCtx context.Context

	clientMu sync.Mutex
	client   *cli.Client

	connStateHandler  js.Value
	connStateHandlerM sync.Mutex
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rootCtx = ctx

	js.Global().Set("harness", js.ValueOf(map[string]any{
		"connect":           js.FuncOf(harnessConnect),
		"submit":            js.FuncOf(harnessSubmit),
		"list":              js.FuncOf(harnessList),
		"snapshot":          js.FuncOf(harnessSnapshot),
		"cancel":            js.FuncOf(harnessCancel),
		"watch":             js.FuncOf(harnessWatch),
		"prune":             js.FuncOf(harnessPrune),
		"startInteractive":  js.FuncOf(harnessStartInteractive),
		"sendInteractive":   js.FuncOf(harnessSendInteractive),
		"resizeInteractive": js.FuncOf(harnessResizeInteractive),
		"detachInteractive": js.FuncOf(harnessDetachInteractive),
		"attachSession":        js.FuncOf(harnessAttachSession),
		"onConnectionChange":   js.FuncOf(harnessOnConnectionChange),
		"fileLs":               js.FuncOf(harnessFileLs),
		"fileDelete":           js.FuncOf(harnessFileDelete),
		"filePushBytes":        js.FuncOf(harnessFilePushBytes),
		"filePullBytes":        js.FuncOf(harnessFilePullBytes),
		"serverDialRunner":     js.FuncOf(harnessServerDialRunner),
	}))

	slog.Info("harness-webui-wasm started")
	select {} // keep runtime alive
}

// rejectErr wraps a Go error as a JS Error and rejects the Promise with it.
// Centralised so every call site produces the same { message } shape on the
// JS side.
func rejectErr(reject js.Value, err error) {
	reject.Invoke(js.Global().Get("Error").New(err.Error()))
}

// currentClient returns the connected *cli.Client or an explanatory error if
// harness.connect has not yet been called. Every harness.* method that needs
// a live connection short-circuits with this.
func currentClient() (*cli.Client, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client == nil {
		return nil, errors.New("not connected; call harness.connect first")
	}
	return client, nil
}

// harnessConnect parses the CID string and dials the server.
//
//	harness.connect("ws:127.0.0.1:8539-*"):                 one-shot, persist=false (compat)
//	harness.connect("ws:...", { persist: true, pingInterval: "15s" }):
//	                                                         options bag, persist defaults to true
func harnessConnect(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 1 {
				rejectErr(reject, errors.New("connect: missing CID arg"))
				return
			}
			cidStr := args[0].String()
			cid, err := objproto.ParseConnectionID(cidStr,
				objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
			if err != nil {
				rejectErr(reject, fmt.Errorf("parse cid: %w", err))
				return
			}

			persist := false
			pingInterval := 15 * time.Second
			if len(args) >= 2 && args[1].Type() == js.TypeObject {
				persist = true // options-bag form defaults to persist:true
				if v := args[1].Get("persist"); v.Type() == js.TypeBoolean {
					persist = v.Bool()
				}
				if v := args[1].Get("pingInterval"); v.Type() == js.TypeString {
					if d, err := time.ParseDuration(v.String()); err == nil {
						pingInterval = d
					}
				}
			}
			_ = pingInterval // peer.DialConfig.PingInterval default (15s) is used; future hook for override

			started := make(chan struct{})
			var startedOnce sync.Once
			peerCIDLocal := cid

			go func() {
				err := cli.PersistLoop(rootCtx,
					func(dialCtx context.Context) (cli.PersistHandle, error) {
						c, derr := cli.Dial(dialCtx, peerCIDLocal)
						if derr != nil {
							return nil, derr
						}
						return cli.NewClientHandle(c), nil
					},
					func(runCtx context.Context, h cli.PersistHandle) error {
						handle := h.(*cli.ClientHandle)
						if err := handle.C.SayHello(runCtx, protocol.ClientKind_Webui); err != nil {
							return err
						}
						clientMu.Lock()
						client = handle.C
						clientMu.Unlock()
						startedOnce.Do(func() { close(started) })
						<-runCtx.Done()
						clientMu.Lock()
						client = nil
						clientMu.Unlock()
						return nil
					},
					cli.PersistConfig{
						Enabled: persist,
						OnState: func(s cli.PersistState) {
							notifyConnState(s)
						},
					})
				if err != nil && !errors.Is(err, context.Canceled) {
					notifyConnState(cli.PersistState{Phase: cli.PersistPhaseClosed, LastError: err})
				}
			}()

			select {
			case <-started:
				resolve.Invoke(js.ValueOf(map[string]any{}))
			case <-rootCtx.Done():
				rejectErr(reject, rootCtx.Err())
			case <-time.After(30 * time.Second):
				rejectErr(reject, errors.New("connect: initial dial timed out (still retrying in background if persist=true)"))
			}
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

func notifyConnState(s cli.PersistState) {
	connStateHandlerM.Lock()
	h := connStateHandler
	connStateHandlerM.Unlock()
	if h.IsUndefined() || h.IsNull() {
		return
	}
	phaseStr := "connecting"
	switch s.Phase {
	case cli.PersistPhaseConnected:
		phaseStr = "connected"
	case cli.PersistPhaseReconnecting:
		phaseStr = "reconnecting"
	case cli.PersistPhaseClosed:
		phaseStr = "closed"
	}
	payload := map[string]any{
		"phase":   phaseStr,
		"attempt": s.Attempt,
	}
	if s.NextRetry > 0 {
		payload["nextRetryMs"] = s.NextRetry.Milliseconds()
	}
	if s.LastError != nil {
		payload["error"] = s.LastError.Error()
	}
	h.Invoke(js.ValueOf(payload))
}

// harnessOnConnectionChange registers a JS callback for connection state changes.
//
//	harness.onConnectionChange((state) => { ... })
func harnessOnConnectionChange(this js.Value, args []js.Value) any {
	if len(args) >= 1 && args[0].Type() == js.TypeFunction {
		connStateHandlerM.Lock()
		connStateHandler = args[0]
		connStateHandlerM.Unlock()
	}
	return nil
}

// harnessSubmit submits a task and resolves with the server-assigned task id.
// An optional "host" field pins the task to a specific runner by hostname.
//
//	harness.submit({repo: "/abs/path", task: "...", host: "raspi"}) -> Promise<taskIDHex>
func harnessSubmit(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("submit: missing options arg"))
				return
			}
			opts := args[0]
			repo := opts.Get("repo").String()
			task := opts.Get("task").String()
			hostVal := opts.Get("host")
			host := ""
			if hostVal.Type() == js.TypeString {
				host = hostVal.String()
			}
			extraArgs := jsArrayToStringSlice(opts.Get("claudeArgs"))
			resumeVal := opts.Get("resumeTaskId")
			resumeTaskID := ""
			if resumeVal.Type() == js.TypeString {
				resumeTaskID = resumeVal.String()
			}
			sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
			if err != nil {
				rejectErr(reject, fmt.Errorf("submit: selector: %w", err))
				return
			}
			id, err := c.SubmitWithSelectorAndArgs(rootCtx, repo, task, sel, extraArgs, resumeTaskID)
			if err != nil {
				rejectErr(reject, fmt.Errorf("submit: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(id))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessList returns the list output as a string.
//
//	harness.list() -> Promise<string>
func harnessList(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			var buf bytesBuffer
			if err := c.List(rootCtx, &buf); err != nil {
				rejectErr(reject, fmt.Errorf("list: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(buf.String()))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessSnapshot returns the current runners + tasks as a JS object,
// shaped for direct consumption by the webui. Strings are pre-decoded,
// TaskIDs are pre-hexed, and statuses are stringified so the JS side does
// not need a label table.
//
//	harness.snapshot() -> Promise<{
//	  runners: [{hostname, status, tasks, maxTasks, roots, connectedAt, lastSeen}],
//	  tasks:   [{id, status, kind, repoPath, prompt, assignedTo, exitCode,
//	             createdAt, startedAt, endedAt}]
//	}>
func harnessSnapshot(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			lr, err := c.Snapshot(rootCtx)
			if err != nil {
				rejectErr(reject, fmt.Errorf("snapshot: %w", err))
				return
			}
			runners := make([]any, 0, len(lr.Runners))
			for _, r := range lr.Runners {
				roots := make([]any, 0, len(r.AllowedRoots))
				for _, root := range r.AllowedRoots {
					roots = append(roots, string(root.Path))
				}
				runners = append(runners, map[string]any{
					"hostname":    string(r.Hostname),
					"status":      r.Status.String(),
					"tasks":       float64(r.ActiveTasksLen),
					"maxTasks":    float64(r.MaxTasks),
					"roots":       roots,
					"connectedAt": float64(r.ConnectedAt),
					"lastSeen":    float64(r.LastSeen),
				})
			}
			tasks := make([]any, 0, len(lr.Tasks))
			for _, t := range lr.Tasks {
				tasks = append(tasks, map[string]any{
					"id":         hex.EncodeToString(t.Id.Id[:]),
					"status":     t.Status.String(),
					"kind":       t.Kind.String(),
					"repoPath":   string(t.RepoPath),
					"prompt":     string(t.Prompt),
					"assignedTo": protocol.RunnerIDToConnID(t.AssignedTo).String(),
					"exitCode":   float64(t.ExitCode),
					"createdAt":  float64(t.CreatedAt),
					"startedAt":  float64(t.StartedAt),
					"endedAt":    float64(t.EndedAt),
				})
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"runners": runners,
				"tasks":   tasks,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessCancel cancels a queued/running task.
//
//	harness.cancel("0123abcd...") -> Promise<void>
func harnessCancel(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("cancel: missing taskID arg"))
				return
			}
			taskIDHex := args[0].String()
			if err := c.Cancel(rootCtx, taskIDHex); err != nil {
				rejectErr(reject, fmt.Errorf("cancel: %w", err))
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessServerDialRunner asks the connected server to reverse-dial a
// Listen-mode runner (Phase A / Phase B relay). Used in ACL environments
// where the runner cannot dial the server directly.
//
//	harness.serverDialRunner(runnerCID)              -> Promise<string>  // direct dial
//	harness.serverDialRunner(runnerCID, viaCIDStr)   -> Promise<string>  // relayed via
func harnessServerDialRunner(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("serverDialRunner: missing runnerCID arg"))
				return
			}
			runnerCIDStr := args[0].String()
			targetCID, err := objproto.ParseConnectionID(runnerCIDStr,
				objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
			if err != nil {
				rejectErr(reject, fmt.Errorf("serverDialRunner: parse runner CID: %w", err))
				return
			}
			var viaCID objproto.ConnectionID
			if len(args) >= 2 && args[1].Type() == js.TypeString {
				if v := strings.TrimSpace(args[1].String()); v != "" {
					viaCID, err = objproto.ParseConnectionID(v,
						objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
					if err != nil {
						rejectErr(reject, fmt.Errorf("serverDialRunner: parse --via: %w", err))
						return
					}
				}
			}
			resp, err := cli.ServerDialRunnerWith(rootCtx, c,
				protocol.ConnIDToRunnerID(targetCID),
				protocol.ConnIDToRunnerID(viaCID))
			if err != nil {
				rejectErr(reject, fmt.Errorf("serverDialRunner: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(resp.Status.String()))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessWatch starts a watch goroutine. Events are pushed via
// window.harness_onTaskEvent(jsonString). The Promise resolves once the
// watch goroutine has been kicked off; the goroutine itself runs until
// rootCtx is cancelled (page unload) or cli.Watch returns an error.
//
//	harness.watch() -> Promise<void>
func harnessWatch(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			pipe := &watchPipe{}
			go func() {
				if err := c.Watch(rootCtx, pipe); err != nil {
					slog.Error("watch ended", "err", err)
				}
			}()
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessPrune asks the server to forget terminal tasks older than the
// given duration string (e.g. "168h"). Resolves with the cli.Prune
// human-readable summary text.
//
//	harness.prune({before: "168h"}) -> Promise<string>
func harnessPrune(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("prune: missing options arg"))
				return
			}
			beforeStr := args[0].Get("before").String()
			before, err := time.ParseDuration(beforeStr)
			if err != nil {
				rejectErr(reject, fmt.Errorf("invalid before duration: %w", err))
				return
			}
			var buf bytesBuffer
			if err := c.Prune(rootCtx, before, nil, false, &buf); err != nil {
				rejectErr(reject, fmt.Errorf("prune: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(buf.String()))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessStartInteractive opens a fresh interactive PTY session for a repo
// and resolves with the server-allocated task id (hex). The signature
// mirrors cli.Interactive (native+wasm) — the server allocates the TaskID
// from OpenInteractiveRequest{RepoPath}, so JS supplies the repo and gets
// the taskID back, not the other way around. An optional "host" field pins
// the session to a specific runner by hostname.
//
//	harness.startInteractive({repo: "/abs/path", host: "raspi"}) -> Promise<taskIDHex>
func harnessStartInteractive(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("startInteractive: missing options arg"))
				return
			}
			opts := args[0]
			repo := opts.Get("repo").String()
			resumeVal := opts.Get("resumeTaskId")
			resumeTaskID := ""
			if resumeVal.Type() == js.TypeString {
				resumeTaskID = resumeVal.String()
			}
			// repo is only required when not resuming — on resume, the
			// server uses the existing TaskEntry's RepoPath.
			if repo == "" && resumeTaskID == "" {
				rejectErr(reject, errors.New("startInteractive: opts.repo is required (unless opts.resumeTaskId is set)"))
				return
			}
			hostVal := opts.Get("host")
			host := ""
			if hostVal.Type() == js.TypeString {
				host = hostVal.String()
			}
			extraArgs := jsArrayToStringSlice(opts.Get("claudeArgs"))
			detachableVal := opts.Get("detachable")
			detachable := false
			if detachableVal.Type() == js.TypeBoolean {
				detachable = detachableVal.Bool()
			}
			sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
			if err != nil {
				rejectErr(reject, fmt.Errorf("startInteractive: selector: %w", err))
				return
			}
			taskID, err := c.InteractiveWithSelectorAndArgs(rootCtx, repo, sel, extraArgs, resumeTaskID, detachable)
			if err != nil {
				rejectErr(reject, fmt.Errorf("interactive: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(taskID))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessSendInteractive forwards user keystrokes (xterm.onData) to the
// active interactive stream. Synchronous: returns true on success, false if
// no session is active or write failed (error logged via slog).
//
//	harness.sendInteractive(stringOrUint8Array) -> bool
func harnessSendInteractive(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf(false)
	}
	val := args[0]
	var data []byte
	switch val.Type() {
	case js.TypeString:
		data = []byte(val.String())
	default:
		// Uint8Array path. We must not pass a non-typed-array to
		// js.CopyBytesToGo; xterm.onData typically yields strings, but
		// xterm-addon-attach style callers may pass Uint8Array.
		length := val.Get("length").Int()
		data = make([]byte, length)
		js.CopyBytesToGo(data, val)
	}
	if err := cli.SendInteractive(data); err != nil {
		slog.Error("sendInteractive", "err", err)
		return js.ValueOf(false)
	}
	return js.ValueOf(true)
}

// harnessResizeInteractive forwards a window-size change to the runner.
// Accepts {cols, rows} as numeric JS fields; non-positive values are
// rejected (returns false) to avoid sending a degenerate Control frame.
//
//	harness.resizeInteractive({cols: 80, rows: 24}) -> bool
func harnessResizeInteractive(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf(false)
	}
	opts := args[0]
	cols := opts.Get("cols").Int()
	rows := opts.Get("rows").Int()
	if cols <= 0 || rows <= 0 {
		return js.ValueOf(false)
	}
	if err := cli.ResizeInteractive(uint16(cols), uint16(rows)); err != nil {
		slog.Error("resizeInteractive", "err", err)
		return js.ValueOf(false)
	}
	return js.ValueOf(true)
}

// harnessDetachInteractive closes the active interactive session, if any.
// Idempotent. Used by the page on tab unload or an explicit Detach button.
//
//	harness.detachInteractive() -> undefined
func harnessDetachInteractive(this js.Value, args []js.Value) any {
	cli.DetachInteractive()
	return js.Undefined()
}

// harnessAttachSession re-attaches the browser xterm to an existing detachable
// interactive session. The stream is acquired via AttachSession RPC and installed
// as the singleton activeInteractiveSession; replayed bytes + live output flow
// through harness_xtermWrite automatically.
//
//	harness.attachSession(taskIDHex) -> Promise<taskIDHex>
func harnessAttachSession(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("attachSession: missing taskIDHex arg"))
				return
			}
			taskIDHex := args[0].String()
			if taskIDHex == "" {
				rejectErr(reject, errors.New("attachSession: taskIDHex is empty"))
				return
			}
			resultID, err := c.AttachSession(rootCtx, taskIDHex)
			if err != nil {
				rejectErr(reject, fmt.Errorf("attachSession: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(resultID))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// bytesBuffer is a minimal io.Writer used for collecting cli output before
// returning it to JS as a single string. We avoid pulling in bytes.Buffer
// just to dodge any potential growth in wasm bundle size; this is a string-
// safe append-only buffer.
type bytesBuffer struct {
	buf []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *bytesBuffer) String() string { return string(b.buf) }

// watchPipe wraps each line written by cli.Watch in a small JSON object and
// forwards it to window.harness_onTaskEvent. cli.Watch emits one human-
// readable line per event terminated with '\n' (see drainTaskEvents /
// jsArrayToStringSlice converts a JS Array of strings (or undefined / null /
// non-array) into a Go []string. Non-string entries are coerced via
// String() so a value typed as e.g. a Number still produces sensible output;
// nil / undefined / empty arrays yield nil so the wire ExtraArgs field stays
// empty (no allocation, no length-prefix payload).
func jsArrayToStringSlice(v js.Value) []string {
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	// Treat non-array (e.g. accidentally passed string) as nil rather than
	// panicking on .Index — caller mistakes shouldn't drop the whole RPC.
	if v.Type() != js.TypeObject {
		return nil
	}
	n := v.Length()
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		entry := v.Index(i)
		if entry.IsUndefined() || entry.IsNull() {
			continue
		}
		out = append(out, entry.String())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// drainRunnerEvents in cli/watch.go). The JS side parses {"line": ...} and
// can render or further parse as needed.
type watchPipe struct {
	carry []byte
}

func (w *watchPipe) Write(p []byte) (int, error) {
	w.carry = append(w.carry, p...)
	for {
		idx := -1
		for i, b := range w.carry {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx == -1 {
			break
		}
		line := string(w.carry[:idx])
		w.carry = w.carry[idx+1:]
		evt := map[string]any{"line": line}
		blob, _ := json.Marshal(evt)
		js.Global().Call("harness_onTaskEvent", string(blob))
	}
	return len(p), nil
}

// rejectFileErr is the file-ops sibling of rejectErr: in addition to the
// Error.message, it stamps a `code` property on the rejection so the JS
// side can branch on the underlying FileTransferStatus without having
// to string-match the error message. Recognised codes:
//
//   already_exists  – push destination present, retry with force=true
//   not_found       – source missing on the runner side
//   path_invalid    – worktree-escape attempt or empty path
//   not_a_directory – delete/dir-delete called on a wrong kind of path
//   not_empty       – dir_delete without recursive on a non-empty dir
//
// All other ack codes (io_error / canceled / internal / etc.) reject
// with code="error" so the JS catch can fall through to a generic
// failure path. Non-FileAckError errors also use code="error".
func rejectFileErr(reject js.Value, err error) {
	code := "error"
	var fe *cli.FileAckError
	if errors.As(err, &fe) {
		switch fe.Status {
		case protocol.FileTransferStatus_AlreadyExists:
			code = "already_exists"
		case protocol.FileTransferStatus_NotFound:
			code = "not_found"
		case protocol.FileTransferStatus_PathInvalid:
			code = "path_invalid"
		case protocol.FileTransferStatus_NotADirectory:
			code = "not_a_directory"
		case protocol.FileTransferStatus_IsDirectory:
			code = "is_directory"
		case protocol.FileTransferStatus_NotEmpty:
			code = "not_empty"
		}
	}
	errObj := js.Global().Get("Error").New(err.Error())
	errObj.Set("code", code)
	reject.Invoke(errObj)
}

// harnessFileLs lists entries directly under taskID's worktree at
// relPath. Returns the same shape harness.snapshot() runners use for
// roots: a plain JS array of {name, size, mode, isDir}.
//
//	harness.fileLs(taskID, relPath) -> Promise<Array<{name, size, mode, isDir}>>
func harnessFileLs(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 2 {
				rejectErr(reject, errors.New("fileLs: missing taskID / relPath args"))
				return
			}
			taskID := args[0].String()
			rel := args[1].String()
			entries, err := c.ListFiles(rootCtx, taskID, rel)
			if err != nil {
				rejectFileErr(reject, err)
				return
			}
			out := make([]any, 0, len(entries))
			for _, e := range entries {
				out = append(out, map[string]any{
					"name":  e.Name,
					"size":  float64(e.Size), // js Number; fine for files <2^53
					"mode":  float64(e.Mode),
					"isDir": e.IsDir,
				})
			}
			resolve.Invoke(js.ValueOf(out))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFileDelete removes a path from taskID's worktree. recursive=true
// switches to the dir_delete direction (empty-dir or rm-rf depending on
// force). For files leave recursive=false; force is ignored on the
// single-file delete path.
//
//	harness.fileDelete(taskID, relPath, recursive, force) -> Promise<void>
func harnessFileDelete(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 2 {
				rejectErr(reject, errors.New("fileDelete: missing taskID / relPath args"))
				return
			}
			taskID := args[0].String()
			rel := args[1].String()
			recursive := len(args) >= 3 && args[2].Truthy()
			force := len(args) >= 4 && args[3].Truthy()
			if recursive {
				err = c.FileDeleteDir(rootCtx, taskID, rel, force)
			} else {
				err = c.FileDelete(rootCtx, taskID, rel)
			}
			if err != nil {
				rejectFileErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFilePushBytes uploads data (a Uint8Array of file contents)
// into taskID's worktree at remoteRel. force=true overwrites; false
// rejects with code="already_exists" if the destination is present,
// letting the JS layer drive a confirm() prompt before retrying.
//
//	harness.filePushBytes(taskID, remoteRel, data, force) -> Promise<void>
func harnessFilePushBytes(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		// Copy bytes out of the Uint8Array on the JS heap into a Go
		// []byte while we're still on the main thread; the goroutine
		// below cannot reach JS values directly.
		if len(args) < 4 {
			rejectErr(reject, errors.New("filePushBytes: missing taskID / remoteRel / data / force args"))
			return nil
		}
		taskID := args[0].String()
		remoteRel := args[1].String()
		dataJS := args[2]
		force := args[3].Truthy()
		length := dataJS.Length()
		data := make([]byte, length)
		js.CopyBytesToGo(data, dataJS)
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if err := c.FilePushBytes(rootCtx, taskID, data, remoteRel, force); err != nil {
				rejectFileErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFilePullBytes fetches remoteRel from taskID's worktree and
// resolves with a Uint8Array of the file contents. The JS layer wraps
// the bytes in a Blob and triggers a download to save them.
//
//	harness.filePullBytes(taskID, remoteRel) -> Promise<Uint8Array>
func harnessFilePullBytes(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 2 {
				rejectErr(reject, errors.New("filePullBytes: missing taskID / remoteRel args"))
				return
			}
			taskID := args[0].String()
			remoteRel := args[1].String()
			data, err := c.FilePullBytes(rootCtx, taskID, remoteRel)
			if err != nil {
				rejectFileErr(reject, err)
				return
			}
			out := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(out, data)
			resolve.Invoke(out)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}
