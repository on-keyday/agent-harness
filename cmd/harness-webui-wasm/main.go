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
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	"github.com/on-keyday/objtrsf/objproto"
)

var (
	rootCtx context.Context

	clientMu sync.Mutex
	client   *cli.Client

	connStateHandler  js.Value
	connStateHandlerM sync.Mutex
)

// clientKindLower mirrors cli.originStr's non-"-" form: lowercase kind name,
// or "" for Unspecified so the page can omit the field. Used for the task
// snapshot's origin / resumed-by attribution.
func clientKindLower(k protocol.ClientKind) string {
	if k == protocol.ClientKind_Unspecified {
		return ""
	}
	return strings.ToLower(k.String())
}

// creatorShort returns the first 8 hex chars of a creator task id, or "" when
// the task was not created by an agent (zero id).
func creatorShort(t protocol.TaskID) string {
	if t.Id == ([16]byte{}) {
		return ""
	}
	return hex.EncodeToString(t.Id[:])[:8]
}

// creatorFull is the untruncated form of creatorShort ("" for the zero id).
func creatorFull(t protocol.TaskID) string {
	if t.Id == ([16]byte{}) {
		return ""
	}
	return hex.EncodeToString(t.Id[:])
}

// outputIdleMs maps the wire pair (last_output_at, output_idle_ms) to the JS
// convention: the server-computed idle age in ms, or -1 when the task has no
// live interactive session output (last_output_at == 0, where output_idle_ms
// would ambiguously read as "0ms ago").
func outputIdleMs(t protocol.TaskInfo) float64 {
	if t.LastOutputAt == 0 {
		return -1
	}
	return float64(t.OutputIdleMs)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rootCtx = ctx

	js.Global().Set("harness", js.ValueOf(map[string]any{
		"connect":            js.FuncOf(harnessConnect),
		"submit":             js.FuncOf(harnessSubmit),
		"list":               js.FuncOf(harnessList),
		"snapshot":           js.FuncOf(harnessSnapshot),
		"gridSet":            js.FuncOf(harnessGridSet),
		"previewStart":       js.FuncOf(harnessPreviewStart),
		"streamStart":        js.FuncOf(harnessStreamStart),
		"streamStop":         js.FuncOf(harnessStreamStop),
		"streamTurn":         js.FuncOf(harnessStreamTurn),
		"streamApprove":      js.FuncOf(harnessStreamApprove),
		"streamInterrupt":    js.FuncOf(harnessStreamInterrupt),
		"streamFinish":       js.FuncOf(harnessStreamFinish),
		"previewStop":        js.FuncOf(harnessPreviewStop),
		"previewInput":       js.FuncOf(harnessPreviewInput),
		"cancel":             js.FuncOf(harnessCancel),
		"watch":              js.FuncOf(harnessWatch),
		"prune":              js.FuncOf(harnessPrune),
		"startInteractive":   js.FuncOf(harnessStartInteractive),
		"sendInteractive":    js.FuncOf(harnessSendInteractive),
		"resizeInteractive":  js.FuncOf(harnessResizeInteractive),
		"detachInteractive":  js.FuncOf(harnessDetachInteractive),
		"attachSession":      js.FuncOf(harnessAttachSession),
		"onConnectionChange": js.FuncOf(harnessOnConnectionChange),
		"fileLs":             js.FuncOf(harnessFileLs),
		"fileDelete":         js.FuncOf(harnessFileDelete),
		"fileMkdir":          js.FuncOf(harnessFileMkdir),
		"filePushBytes":      js.FuncOf(harnessFilePushBytes),
		"filePullBytes":      js.FuncOf(harnessFilePullBytes),
		"filePullBytesRange": js.FuncOf(harnessFilePullBytesRange),
		"filePullDirBytes":   js.FuncOf(harnessFilePullDirBytes),
		"fileEditLoad":       js.FuncOf(harnessFileEditLoad),
		"fileEditCommit":     js.FuncOf(harnessFileEditCommit),
		"fileEditEncode":     js.FuncOf(harnessFileEditEncode),
		"gitQuery":           js.FuncOf(harnessGitQuery),
		"serverDialRunner":   js.FuncOf(harnessServerDialRunner),
		"sendNotification":   js.FuncOf(harnessSendNotification),
		"awaitIdle":          js.FuncOf(harnessAwaitIdle),
		"watchNotifications": js.FuncOf(harnessWatchNotifications),
		"capList":            js.FuncOf(harnessCapList),
		"scopeForms":         js.FuncOf(harnessScopeForms),
		"scopeSpec":          js.FuncOf(harnessScopeSpec),
		"setCaps":            js.FuncOf(harnessSetCaps),
		"setParent":          js.FuncOf(harnessSetParent),
		"boardTopics":        js.FuncOf(harnessBoardTopics),
		"boardRead":          js.FuncOf(harnessBoardRead),
		"boardPurge":         js.FuncOf(harnessBoardPurge),
		"boardSubscribers":   js.FuncOf(harnessBoardSubscribers),
		"forwardKill":        js.FuncOf(harnessForwardKill),
		"rawOpen":            js.FuncOf(harnessRawOpen),
		"rawSend":            js.FuncOf(harnessRawSend),
		"rawSendHTTP":        js.FuncOf(harnessRawSendHTTP),
		"rawClose":           js.FuncOf(harnessRawClose),
		"httpFetch":          js.FuncOf(harnessHTTPFetch),
		"previewPinOpen":     js.FuncOf(harnessPreviewPinOpen),
		"previewPinFetch":    js.FuncOf(harnessPreviewPinFetch),
		"previewPinClose":    js.FuncOf(harnessPreviewPinClose),
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

// waitForClient blocks until a live client handle is installed and returns it,
// or returns ctx.Err() once ctx is done.
//
// Reconnect ordering: PersistLoop emits Connected (cli/persist.go) — which the
// JS layer turns into an onConnectionChange('connected') that re-invokes the
// watch starters below — BEFORE onConnect installs the new handle into `client`
// and completes SayHello. So during the reconnect window currentClient() is
// transiently nil. The stream starters must WAIT for the fresh handle rather
// than snapshot-once-and-give-up; otherwise a reconnect permanently kills the
// stream (e.g. the notification feed stops updating until a full page reload).
// First connect avoids this because the starters are registered only after
// `await harness.connect()` resolves, which already gates on `client` being set.
func waitForClient(ctx context.Context) (*cli.Client, error) {
	for {
		clientMu.Lock()
		c := client
		clientMu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
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
						c, derr := cli.Dial(dialCtx, peerCIDLocal, protocol.ClientKind_Webui)
						if derr != nil {
							return nil, derr
						}
						return cli.NewClientHandle(c), nil
					},
					func(runCtx context.Context, h cli.PersistHandle) error {
						handle := h.(*cli.ClientHandle)
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

// scopeFromOpts reads the optional `scope` string off a JS options object and
// parses it with the same cli.ParseScope the CLI and TUI use, so the three
// surfaces cannot drift on the grammar. Absent or empty = the subtree default.
// scopeIDStrings renders a scope's explicit id list as hex strings for the
// snapshot JSON ([]any so js.ValueOf accepts it).
func scopeIDStrings(s protocol.TaskScope) []any {
	out := make([]any, 0, len(s.Ids))
	for _, id := range s.Ids {
		out = append(out, hex.EncodeToString(id.Id[:]))
	}
	return out
}

func scopeVisIDStrings(s protocol.TaskScope) []any {
	out := make([]any, 0, len(s.VisIds))
	for _, id := range s.VisIds {
		out = append(out, hex.EncodeToString(id.Id[:]))
	}
	return out
}

// visBaseString renders the visibility rank for the dialog's radio row, with ""
// meaning NOT STATED. The zero ScopeBase is a legal rank (subtree), so the
// presence bit is the only thing that separates "follows the action base" from
// "explicitly subtree" — reading VisBase without it would turn every default
// into an explicit rank the moment a dialog applied.
func visBaseString(s protocol.TaskScope) string {
	if !s.VisBasePresent() {
		return ""
	}
	return s.VisBase.String()
}

// overridesFromOpts reads the optional `scopeFor` array off a JS options
// object: each entry is a "CAPS=SCOPE" string in the --scope-for spelling, so
// the browser sends what the CLI accepts and the parse/merge rules — including
// the disjointness check — live in one place rather than being re-implemented
// in JS.
// overrideSpecs renders each override in the "CAPS=SCOPE" spelling that
// overridesFromOpts reads back, so the browser can echo what it was given.
func overrideSpecs(in []protocol.ScopeOverride) []any {
	out := make([]any, 0, len(in))
	for _, o := range in {
		sc := protocol.TaskScope{Base: o.Base, Ids: o.Ids, IdsLen: o.IdsLen}
		sc.SetExcludeSelf(o.ExcludeSelf())
		out = append(out, cli.CapsLabel(o.Caps)+"="+cli.ScopeLabel(sc))
	}
	return out
}

// scopeByCapJS is the resolved capability -> scope map, merge already done, so
// the browser never re-derives which override covers a bit.
func scopeByCapJS(caps protocol.Capability, base protocol.TaskScope, ov []protocol.ScopeOverride) map[string]any {
	resolved := cli.ResolvedScopeByCap(caps, base, ov)
	out := make(map[string]any, len(resolved))
	for k, v := range resolved {
		out[k] = v
	}
	return out
}

func overridesFromOpts(opts js.Value) ([]protocol.ScopeOverride, error) {
	v := opts.Get("scopeFor")
	if v.Type() != js.TypeObject || v.Length() == 0 {
		return nil, nil
	}
	var out []protocol.ScopeOverride
	for i := 0; i < v.Length(); i++ {
		e := v.Index(i)
		if e.Type() != js.TypeString || e.String() == "" {
			continue
		}
		_, ov, err := cli.ParseScopeFor(e.String())
		if err != nil {
			return nil, err
		}
		merged, err := cli.MergeScopeOverride(out, ov)
		if err != nil {
			return nil, err
		}
		out = merged
	}
	return out, nil
}

func scopeFromOpts(opts js.Value) (protocol.TaskScope, error) {
	sv := opts.Get("scope")
	if sv.Type() != js.TypeString || sv.String() == "" {
		return protocol.TaskScope{Base: protocol.ScopeBase_Subtree}, nil
	}
	sc, err := cli.ParseScope(sv.String())
	if err != nil {
		return protocol.TaskScope{}, fmt.Errorf("scope: %w", err)
	}
	return sc, nil
}

// harnessScopeSpec serializes an edited scope to the --scope grammar via
// cli.ScopeSpec, so the browser holds no copy of it.
//
//	harness.scopeSpec({base, excludeSelf, ids, carry}) -> string
//
// carry is the task's current scope string on a re-grant ("" on a spawn); only
// its visibility half is kept. Throws on a bad base word or id, which is what
// the caller wants: a silently-wrong scope is the failure this replaced.
func harnessScopeSpec(this js.Value, args []js.Value) any {
	if len(args) == 0 || args[0].Type() != js.TypeObject {
		panic(js.Error{Value: js.ValueOf("scopeSpec: want an options object")})
	}
	opts := args[0]
	base := "subtree"
	if v := opts.Get("base"); v.Type() == js.TypeString {
		base = v.String()
	}
	excludeSelf := opts.Get("excludeSelf").Truthy()
	// "" is the third state of the visibility radio, not a missing argument:
	// the rank is unstated and follows the action base.
	visBase := ""
	if v := opts.Get("visBase"); v.Type() == js.TypeString {
		visBase = v.String()
	}
	strList := func(key string) []string {
		var out []string
		if v := opts.Get(key); v.Type() == js.TypeObject {
			for i := 0; i < v.Length(); i++ {
				out = append(out, v.Index(i).String())
			}
		}
		return out
	}
	spec, err := cli.ScopeSpec(base, excludeSelf, strList("ids"), visBase, strList("visIds"))
	if err != nil {
		panic(js.Error{Value: js.ValueOf(err.Error())})
	}
	return js.ValueOf(spec)
}

// harnessScopeForms returns the --scope syntaxes with their descriptions, so
// the spawn dialog can offer them without hardcoding a second copy of the
// grammar.
//
//	harness.scopeForms() -> [{syntax: string, description: string}, ...]
func harnessScopeForms(this js.Value, args []js.Value) any {
	var out []any
	for _, si := range cli.ScopesCatalog() {
		out = append(out, map[string]any{"syntax": si.Syntax, "description": si.Description})
	}
	return js.ValueOf(out)
}

// harnessSetCaps re-grants a live task's caps and/or scope. Operator-only,
// enforced server-side; a WebUI connection is an operator connection by
// construction, so there is no client-side gate here.
//
//	harness.setCaps({taskId, caps?, scope?, cascade?, keepConns?})
//	  -> Promise<{affected: string[], connsClosed: number}>
func harnessSetCaps(this js.Value, args []js.Value) any {
	opts := js.Undefined()
	if len(args) >= 1 && args[0].Type() == js.TypeObject {
		opts = args[0]
	}
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if opts.IsUndefined() {
				rejectErr(reject, fmt.Errorf("setCaps: options object required"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			sc := cli.SetCapsOpts{}
			if tv := opts.Get("taskId"); tv.Type() == js.TypeString {
				sc.TaskID = tv.String()
			}
			if cv := opts.Get("caps"); cv.Type() == js.TypeNumber {
				sc.Caps = cli.CapsPtr(protocol.Capability(uint32(cv.Int())))
			}
			if sv := opts.Get("scope"); sv.Type() == js.TypeString && sv.String() != "" {
				parsed, perr := cli.ParseScope(sv.String())
				if perr != nil {
					rejectErr(reject, fmt.Errorf("setCaps: scope: %w", perr))
					return
				}
				sc.Scope = &parsed
				ov, ovErr := overridesFromOpts(opts)
				if ovErr != nil {
					rejectErr(reject, fmt.Errorf("setCaps: scope-for: %w", ovErr))
					return
				}
				sc.Overrides = ov
			}
			if v := opts.Get("cascade"); v.Type() == js.TypeBoolean {
				sc.Cascade = v.Bool()
			}
			if v := opts.Get("keepConns"); v.Type() == js.TypeBoolean {
				sc.KeepConns = v.Bool()
			}
			res, err := cli.SetCapsWith(rootCtx, c, sc)
			if err != nil {
				rejectErr(reject, fmt.Errorf("setCaps: %w", err))
				return
			}
			affected := make([]any, 0, len(res.Affected))
			for _, id := range res.Affected {
				affected = append(affected, id)
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"affected":    affected,
				"connsClosed": float64(res.ConnsClosed),
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessSetParent re-points a live task's parent link, or swaps the task
// with its current parent. Operator-only, enforced server-side; a WebUI
// connection is an operator connection by construction, so there is no
// client-side gate here. Omitting parentId without swap detaches the task to
// the operator root.
//
//	harness.setParent({taskId, parentId?, swap?})
//	  -> Promise<{oldParent: string, newParent: string, swappedId: string}>  ("" = root)
func harnessSetParent(this js.Value, args []js.Value) any {
	opts := js.Undefined()
	if len(args) >= 1 && args[0].Type() == js.TypeObject {
		opts = args[0]
	}
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if opts.IsUndefined() {
				rejectErr(reject, fmt.Errorf("setParent: options object required"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			sp := cli.SetParentOpts{}
			if tv := opts.Get("taskId"); tv.Type() == js.TypeString {
				sp.TaskID = tv.String()
			}
			if pv := opts.Get("parentId"); pv.Type() == js.TypeString {
				sp.ParentID = pv.String()
			}
			if v := opts.Get("swap"); v.Type() == js.TypeBoolean {
				sp.Swap = v.Bool()
			}
			res, err := cli.SetParentWith(rootCtx, c, sp)
			if err != nil {
				rejectErr(reject, fmt.Errorf("setParent: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"oldParent": res.OldParent,
				"newParent": res.NewParent,
				"swappedId": res.SwappedID,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessCapList returns the granular caps as [{name, bit}] for the UI chips
// (excludes none/all — those are quick-set buttons). Names from Capability.String().
//
//	harness.capList() -> [{name: string, bit: number}, ...]
func harnessCapList(this js.Value, args []js.Value) any {
	var out []any
	for _, c := range cli.GrantableCaps() {
		if c == protocol.Capability_None || c == protocol.Capability_All {
			continue
		}
		out = append(out, map[string]any{"name": c.String(), "bit": float64(uint32(c))})
	}
	return js.ValueOf(out)
}

// harnessSubmit submits a task and resolves with the server-assigned task id.
// An optional "host" field pins the task to a specific runner by hostname.
//
//	harness.submit({repo: "/abs/path", task: "...", host: "raspi", agent: "codex"}) -> Promise<taskIDHex>
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
			// Default-deny, matching ParseCaps(""): a JS caller that omits
			// `caps` spawns a task with no control plane. The WebUI always
			// sends its Compose state, so this is the floor for scripted
			// callers, not the button path.
			caps := protocol.Capability_None
			if cv := opts.Get("caps"); cv.Type() == js.TypeNumber {
				caps = protocol.Capability(uint32(cv.Int()))
			}
			scope, scopeErr := scopeFromOpts(opts)
			overrides, ovErr := overridesFromOpts(opts)
			if ovErr != nil && scopeErr == nil {
				scopeErr = ovErr
			}
			if scopeErr != nil {
				rejectErr(reject, scopeErr)
				return
			}
			resumeCapsOverride := false
			if rcov := opts.Get("resumeCapsOverride"); rcov.Type() == js.TypeBoolean {
				resumeCapsOverride = rcov.Bool()
			}
			scopePresent := false
			if sp := opts.Get("scopePresent"); sp.Type() == js.TypeBoolean {
				scopePresent = sp.Bool()
			}
			resumeConversation := false
			if rc := opts.Get("resumeConversation"); rc.Type() == js.TypeBoolean {
				resumeConversation = rc.Bool()
			}
			// agent selects a named agent profile advertised by the target
			// runner (multi-agent-profile design §6); empty = runner default
			// / (on resume) the resumed task's own profile.
			agentProfile := ""
			if av := opts.Get("agent"); av.Type() == js.TypeString {
				agentProfile = av.String()
			}
			id, err := c.Submit(rootCtx, repo, task, cli.SessionOpts{
				Selector: sel, ExtraArgs: extraArgs, ResumeTaskID: resumeTaskID,
				Caps: caps, Scope: scope, Overrides: overrides,
				ScopePresent: scopePresent, ResumeCapsOverride: resumeCapsOverride,
				ResumeConversation: resumeConversation, AgentProfile: agentProfile,
			})
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

// connRoleLower maps a ConnRole to its lowercase string representation for the
// JS side (mirrors clientKindLower for ClientKind but covers the ConnRole enum
// which additionally has "runner" and "unspecified").
func connRoleLower(r protocol.ConnRole) string {
	if r == protocol.ConnRole_Unspecified {
		return "unspecified"
	}
	return strings.ToLower(r.String())
}

// connRemoteAddr derives the "ip:port" portion from a cid ("transport:ip:port-id")
// for the WebUI's IP-cluster grouping. The cid is the single source of truth for
// the remote address (no separate wire field); ParseConnectionID is the canonical
// reverse of ConnectionID.String(). Returns "" on parse failure (the JS side's
// connIpPart tolerates an empty string).
func connRemoteAddr(cid string) string {
	c, err := objproto.ParseConnectionID(cid, 0)
	if err != nil {
		return ""
	}
	return c.Addr.String()
}

// harnessSnapshot returns the current runners + tasks + connections as a JS
// object, shaped for direct consumption by the webui. Strings are pre-decoded,
// TaskIDs are pre-hexed, and statuses are stringified so the JS side does
// not need a label table.
//
//	harness.snapshot() -> Promise<{
//	  runners:  [{hostname, status, tasks, maxTasks, roots, connectedAt, lastSeen, agentBin, agentProfiles, skillsInjected}],
//	  tasks:    [{id, status, kind, repoPath, prompt, assignedTo, exitCode,
//	              createdAt, startedAt, endedAt, agentProfile, skillsInjected,
//	              viewers, cowriters, errorMsg}],
//	  conns:    [{cid, role, remoteAddr, principalTask, connectedAt, identified}],
//	  forwards: [{forward_id, dir, task, spec, origin}]
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
				// agentProfiles is the runner's advertised named-profile set
				// (multi-agent-profile design §2/§6); the new-session form and
				// each task-sheet's resume agent dropdown populate their
				// options from the union of this across runners.
				profiles := make([]any, 0, len(r.AgentProfiles))
				for _, p := range r.AgentProfiles {
					profiles = append(profiles, string(p.Name))
				}
				runners = append(runners, map[string]any{
					"hostname":       string(r.Hostname),
					"status":         r.Status.String(),
					"tasks":          float64(r.ActiveTasksLen),
					"maxTasks":       float64(r.MaxTasks),
					"roots":          roots,
					"connectedAt":    float64(r.ConnectedAt),
					"lastSeen":       float64(r.LastSeen),
					"agentBin":       string(r.AgentBin),
					"agentProfiles":  profiles,
					"skillsInjected": r.SkillsInjected(),
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
					"origin":     clientKindLower(t.OriginKind),
					"resumedBy":  clientKindLower(t.ResumedByKind),
					"createdBy":  creatorShort(t.CreatorTaskId),
					// createdById is the RAW full id beside the short label:
					// the parent-picker dialog highlights the current parent
					// by exact id (a truncated prefix could match the wrong
					// row) — same label+raw pattern as capsBits/scopeBase.
					"createdById": creatorFull(t.CreatorTaskId),
					"caps":        cli.CapsLabel(t.Capabilities),
					"scope":       cli.ScopeLabel(t.Scope),
					// Raw prefill fields beside the labels: the re-grant
					// dialog seeds its chips/radios/checklist from these —
					// back-parsing label forms like "all,-spawn" would mean
					// re-implementing ParseCaps in JS.
					"capsBits":  float64(uint32(t.Capabilities)),
					"scopeBase": t.Scope.Base.String(),
					"scopeIds":  scopeIDStrings(t.Scope),
					// exclude_self is the action base's other half.
					"scopeExcludeSelf": t.Scope.ExcludeSelf(),
					// The visibility half, raw and in the same shape, because
					// the dialog now edits it rather than carrying it blind.
					// scopeVisBase is "" when the rank is NOT STATED, which is
					// its own value ("visibility follows the action base") and
					// not a synonym for subtree — the radio row needs the
					// three-state distinction the wire keeps in
					// vis_base_present.
					"scopeVisBase": visBaseString(t.Scope),
					"scopeVisIds":  scopeVisIDStrings(t.Scope),
					// scopeFor is the label; scopeForSpecs is the raw
					// "CAPS=SCOPE" list the re-grant dialog sends straight
					// back, so a round trip through the UI cannot lose a
					// narrowing it did not show a control for.
					"scopeFor":      cli.OverridesLabel(t.Overrides),
					"scopeForSpecs": overrideSpecs(t.Overrides),
					"scopeByCap":    scopeByCapJS(t.Capabilities, t.Scope, t.Overrides),
					// agentProfile is the named profile this task last ran under
					// (empty = runner default); the resume action sheet's agent
					// dropdown defaults to this (multi-agent-profile design §4b).
					"agentProfile": string(t.AgentProfile),
					// skillsInjected says the runner this task was assigned to
					// declares it injects the harness skill + inbox hook. It
					// rides on the task, not on the runners array, because a
					// confined caller is served zero runners — see handleList.
					"skillsInjected": t.SkillsInjected(),
					// Observers on the live session, split by what they can do.
					// Independent of the Running/Detached status, which tracks
					// only the CONTROL attach — a task watched through this
					// UI's own preview reads Detached with viewers > 0.
					"viewers":   float64(t.Viewers),
					"cowriters": float64(t.Cowriters),
					// Terminal-failure reason (e.g. "runner_disconnected"); empty
					// for non-failed tasks. Rendered in red on the task card.
					"errorMsg": string(t.ErrorMessage),
					// Server-clock idle age of the live session's PTY output;
					// -1 = no live interactive session / no output yet. JS
					// derives the busy/idle badge from this (never from local
					// clock math — cross-host skew).
					"outputIdleMs": outputIdleMs(t),
				})
			}
			// taskTree is the creator hierarchy WITH its grid positions, both
			// computed here rather than in JS. cli.BuildTaskTree already backs
			// `ls --tree` and the TUI, and cli.TaskTreeLayout is the half that
			// can be subtly wrong (siblings on top of each other, a parent off
			// centre) — Go is where this repo can test that. JS is left a
			// painter: multiply col/depth by a spacing, draw a circle and an
			// edge to parent.
			treeRows := cli.BuildTaskTree(lr.Tasks)
			taskTree := make([]any, 0, len(treeRows))
			for _, n := range cli.TaskTreeLayout(treeRows) {
				taskTree = append(taskTree, map[string]any{
					"id":     n.ID,
					"parent": n.Parent,
					"depth":  float64(n.Depth),
					"col":    n.Col,
					"orphan": n.Orphan,
				})
			}

			// Fetch the live connection list using the same long-lived client
			// (Pitfall 3 / feedback_reuse_long_lived_client: never dial+close here).
			// If ConnListWith fails (e.g. server lacks the capability), we return an
			// empty array so the topology section gracefully shows "(none)" rather than
			// breaking the entire snapshot.
			connInfos, connErr := c.ConnListWith(rootCtx)
			if connErr != nil {
				slog.Warn("snapshot: ConnListWith failed (topology will be empty)", "err", connErr)
			}
			conns := make([]any, 0, len(connInfos))
			for _, ci := range connInfos {
				cidStr := string(ci.Cid)
				conns = append(conns, map[string]any{
					"cid":           cidStr,
					"role":          connRoleLower(ci.Role),
					"remoteAddr":    connRemoteAddr(cidStr),
					"principalTask": hex.EncodeToString(ci.PrincipalTask.Id[:]),
					"connectedAt":   float64(ci.ConnectedAt),
					"identified":    ci.Identified(),
				})
			}
			// Fetch the live port-forward registry using the same long-lived
			// client (Pitfall 3: never dial+close here). A failure here must
			// not fail the whole snapshot — log and emit an empty array,
			// exactly as the conns section above degrades.
			fwInfos, fwErr := c.PortForwardListWith(rootCtx, "")
			if fwErr != nil {
				slog.Warn("snapshot: PortForwardListWith failed (forwards will be empty)", "err", fwErr)
			}
			forwards := make([]any, 0, len(fwInfos))
			for i := range fwInfos {
				fi := &fwInfos[i]
				forwards = append(forwards, map[string]any{
					"forward_id": float64(fi.ForwardId),
					"dir":        cli.PortForwardDirFlag(fi.Direction),
					"task":       hex.EncodeToString(fi.TaskId.Id[:]),
					"spec":       cli.PortForwardSpecString(fi),
					// origin is the single "kind cid" string (cli.PortForwardOrigin) —
					// the CLI, TUI, and WebUI all render this exact helper's output so
					// the three surfaces agree on what "origin" means (the cid half is
					// what distinguishes two identical specs started by different
					// clients; a kind-only rendering was Task 6's caught half-reimplementation).
					"origin": cli.PortForwardOrigin(fi),
					// origin_cid is the join key for the topology's forward edges:
					// it matches conns[].cid exactly. `origin` above is the display
					// form ("<kind> <cid>"), and splitting that to recover the cid
					// would make the diagram depend on a formatting convention.
					"origin_cid": string(fi.OriginCid),
				})
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"runners":  runners,
				"tasks":    tasks,
				"taskTree": taskTree,
				"conns":    conns,
				"forwards": forwards,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessGridSet answers the session grid's one question — WHICH tasks — plus
// the label naming that choice.
//
//	harness.gridSet({mode, anchor, ids}) -> Promise<{ids: string[], label: string}>
//	  mode:   "all" | "subtree" | "descendants" | "ids"
//	  anchor: 32-hex task id, for subtree / descendants
//	  ids:    array of 32-hex task ids, for "ids"
//
// Both halves come from cli.GridSet, the same call the TUI's g / z / Z keys and
// its `grid` verb make. The set has to be shared because "who is whose child"
// and "which tasks did this one's scope name" are answered in Go; the LABEL has
// to be shared for a duller reason — two surfaces formatting it themselves is
// how the same set ends up with two names.
//
// What is TILEABLE stays on the JS side (liveInteractiveTasks + the per-session
// toggles), which already owns that predicate for the other entry points.
func harnessGridSet(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 || args[0].Type() != js.TypeObject {
				rejectErr(reject, errors.New("gridSet: pass {mode, anchor, ids}"))
				return
			}
			req := args[0]
			mode, err := cli.ParseGridScopeMode(jsString(req, "mode"))
			if err != nil {
				rejectErr(reject, err)
				return
			}
			var idHexes []string
			if v := req.Get("ids"); v.Type() == js.TypeObject {
				for i := 0; i < v.Length(); i++ {
					idHexes = append(idHexes, v.Index(i).String())
				}
			}
			lr, err := c.Snapshot(rootCtx)
			if err != nil {
				rejectErr(reject, fmt.Errorf("gridSet: %w", err))
				return
			}
			set, label, err := cli.GridSet(lr.Tasks, mode, jsString(req, "anchor"), idHexes)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			ids := make([]any, 0, len(set))
			for _, t := range set {
				ids = append(ids, hex.EncodeToString(t.Id.Id[:]))
			}
			resolve.Invoke(js.ValueOf(map[string]any{"ids": ids, "label": label}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// jsString reads an optional string field, treating a missing/null one as "".
func jsString(o js.Value, key string) string {
	if v := o.Get(key); v.Type() == js.TypeString {
		return v.String()
	}
	return ""
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

// harnessBoardTopics lists all agentboard topics with aggregate metadata.
//
//	harness.boardTopics() -> Promise<[{name, lastSeq, lastPublishedAtMs, msgCount, retractedCount}]>
func harnessBoardTopics(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			topics, err := c.BoardTopics(rootCtx)
			if err != nil {
				rejectErr(reject, fmt.Errorf("boardTopics: %w", err))
				return
			}
			out := make([]any, 0, len(topics))
			for _, t := range topics {
				out = append(out, map[string]any{
					"name": t.Name,
					// Board seq is UnixNano-seeded (~1.9e18), well beyond
					// JS Number.MAX_SAFE_INTEGER (2^53-1). Emit as a decimal
					// string so precision survives the JS boundary; a float64
					// here silently rounds to the nearest ULP (~256).
					"lastSeq":           strconv.FormatUint(t.LastSeq, 10),
					"lastPublishedAtMs": float64(t.LastPublishedAtMs),
					"msgCount":          float64(t.MsgCount),
					// Withdrawn messages (agent retract), counted apart from
					// msgCount so that one keeps meaning "what a subscriber
					// would receive".
					"retractedCount": float64(t.RetractedCount),
				})
			}
			resolve.Invoke(js.ValueOf(out))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessBoardSubscribers lists each task's agentboard subscription set,
// optionally narrowed to the tasks a publish to one topic would reach.
//
//	harness.boardSubscribers(topic?) -> Promise<[{taskId, hostname, agentProfile, patterns}]>
//
// No u64 crosses this boundary, so unlike boardRead/boardTopics nothing here
// needs the decimal-string treatment.
func harnessBoardSubscribers(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		topic := ""
		if len(args) > 0 && args[0].Type() == js.TypeString {
			topic = args[0].String()
		}
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			rows, err := c.BoardSubscribers(rootCtx, topic)
			if err != nil {
				rejectErr(reject, fmt.Errorf("boardSubscribers: %w", err))
				return
			}
			out := make([]any, 0, len(rows))
			for _, r := range rows {
				pats := make([]any, 0, len(r.Patterns))
				for _, pt := range r.Patterns {
					pats = append(pats, map[string]any{
						"name": pt.Name,
						// shown is a board seq, and those are UnixNano-seeded
						// (~1.9e18) -- past Number.MAX_SAFE_INTEGER. Emit it as a
						// decimal string for the same reason lastSeq is: a float64
						// silently rounds to the nearest ULP (~256).
						"shown":   strconv.FormatUint(pt.Shown, 10),
						"pending": float64(pt.Pending),
					})
				}
				out = append(out, map[string]any{
					"taskId": r.TaskHex,
					// Empty hostname means registered but not yet attached;
					// the JS side renders it as "-" rather than hiding the row.
					"hostname":     r.Hostname,
					"agentProfile": r.AgentProfile,
					"patterns":     pats,
				})
			}
			resolve.Invoke(js.ValueOf(out))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessBoardRead returns all retained messages for a topic.
//
//	harness.boardRead(topic) -> Promise<{found, msgs:[{seq,fromTask,fromHostname,agentProfile,replyToTopic,receivedAtMs,payload,retracted,retractedAtMs}]}>
func harnessBoardRead(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("boardRead: missing topic arg"))
				return
			}
			topic := args[0].String()
			msgs, found, err := c.BoardRead(rootCtx, topic)
			if err != nil {
				rejectErr(reject, fmt.Errorf("boardRead: %w", err))
				return
			}
			// The per-message delivery answer is computed HERE, in Go, and
			// shipped as a result rather than as inputs: comparing board seqs
			// in JS needs BigInt (they exceed Number.MAX_SAFE_INTEGER), and a
			// second implementation of the comparison is exactly the mirror
			// this codebase keeps getting bitten by. A subscribers failure is
			// not fatal to the read — the messages still cross, with 0/0.
			subs, serr := c.BoardSubscribers(rootCtx, topic)
			if serr != nil {
				subs = nil
			}
			msgsOut := make([]any, 0, len(msgs))
			for _, m := range msgs {
				shownN, shownTotal := cli.ShownTo(subs, topic, m.Seq)
				msgsOut = append(msgsOut, map[string]any{
					// Decimal string, not float64: board seq exceeds JS's
					// 2^53 safe-integer range and would round, so a purge
					// keyed on the rounded seq never matches server-side.
					"seq": strconv.FormatUint(m.Seq, 10),
					// Also a seq, so also a decimal string — "0" when the
					// message is not a reply.
					"inReplyTo":    strconv.FormatUint(m.InReplyTo, 10),
					"replyToTopic": m.ReplyToTopic,
					"fromTask":     m.FromTaskHex,
					"fromHostname": m.FromHostname,
					"agentProfile": m.FromAgentProfile,
					"receivedAtMs": float64(m.ReceivedAtMs),
					"payload":      string(m.Payload),
					// A message its author withdrew. It reaches no agent any
					// more and exists only on this operator surface, so the
					// flag has to cross the bridge or the WebUI would show a
					// withdrawn message as if it were still live.
					"retracted":     m.Retracted,
					"retractedAtMs": float64(m.RetractedAtMs),
					// How many of the topic's subscribers have been handed this
					// message, out of how many subscribe at all. Small counts,
					// so float64 is safe here — unlike seq.
					"shownTo":      float64(shownN),
					"shownToTotal": float64(shownTotal),
				})
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"found": found,
				"msgs":  msgsOut,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessBoardPurge purges one message (seq != 0) or an entire topic (seq == 0).
//
//	harness.boardPurge(topic, seq) -> Promise<{purged, found}>
func harnessBoardPurge(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("boardPurge: missing topic/seq args"))
				return
			}
			topic := args[0].String()
			// Per-message seq arrives as a decimal string (see boardRead):
			// board seq is UnixNano-seeded and exceeds JS's 2^53 safe-integer
			// range, so a float64 round-trip rounds it and the purge misses the
			// retained message. Whole-topic purge passes a JS number 0. Branch
			// on the JS type: parse the exact u64 from a string, take .Float()
			// only for the (small, exact) numeric case.
			var seq uint64
			if args[1].Type() == js.TypeString {
				parsed, perr := strconv.ParseUint(args[1].String(), 10, 64)
				if perr != nil {
					rejectErr(reject, fmt.Errorf("boardPurge: bad seq %q: %w", args[1].String(), perr))
					return
				}
				seq = parsed
			} else {
				seq = uint64(args[1].Float())
			}
			purged, found, err := c.BoardPurge(rootCtx, topic, seq)
			if err != nil {
				rejectErr(reject, fmt.Errorf("boardPurge: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"purged": float64(purged),
				"found":  found,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessForwardKill closes one registered port forward. Unlike board seq
// (which must travel as a decimal string because it is UnixNano-seeded and
// exceeds JS's 2^53 safe range), forward ids come from a small `next++`
// counter, so a JS number round-trips exactly.
//
//	harness.forwardKill(forwardId) -> Promise<void>
func harnessForwardKill(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("forwardKill: missing forward id"))
				return
			}
			id := uint64(args[0].Float())
			if err := c.KillPortForwardWith(rootCtx, id); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessRawOpen opens a port forward whose client-side endpoint is this page: no
// local listener exists (a browser cannot bind one), the runner dials host:port,
// and bytes arrive via the harness_rawData hook keyed by paneKey. The page mints
// paneKey and may rawClose it while this is still in flight — that supersedes
// the open instead of leaving a registered forward behind.
//
//	harness.rawOpen(paneKey, taskIDHex, host, port) -> Promise<void>
func harnessRawOpen(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 4 {
				rejectErr(reject, errors.New("rawOpen: want (paneKey, taskIDHex, host, port)"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if err := cli.OpenRawPane(rootCtx, c, args[0].String(), args[1].String(), args[2].String(), args[3].Int()); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessRawSend writes bytes to a pane's connection.
//
//	harness.rawSend(key, Uint8Array) -> Promise<void>
func harnessRawSend(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 2 {
				rejectErr(reject, errors.New("rawSend: want (key, Uint8Array)"))
				return
			}
			val := args[1]
			data := make([]byte, val.Get("length").Int())
			js.CopyBytesToGo(data, val)
			if err := cli.SendRawPane(args[0].String(), data); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessRawClose closes a pane's connection, which deregisters the forward.
//
//	harness.rawSendHTTP(key, {method, path, headers, body}) -> Promise<number>
//
// Resolves with the number of bytes written, so the caller can account for a
// request it did not assemble.
//
// headers is a newline-separated string, matching the TUI's textarea: one
// header per line, so neither surface has to invent a separator.
func harnessRawSendHTTP(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 2 || args[1].Type() != js.TypeObject {
				rejectErr(reject, errors.New("rawSendHTTP: want (key, {method, path, headers, body})"))
				return
			}
			spec := httpSpecFromJS(args[1])
			n, err := cli.SendRawPaneHTTP(args[0].String(), spec)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.ValueOf(n))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// httpSpecFromJS reads the {method, path, headers, body} object both HTTP
// bindings take.
//
// headers arrives in two shapes because its two producers genuinely differ:
// the raw pane collects it in a textarea (one header per line, matching the
// TUI, so neither surface has to invent a separator), while the preview shim
// already holds them structured and would have to join them only to have this
// split them again. Accepting both here keeps one parser instead of two that
// could drift apart.
func httpSpecFromJS(o js.Value) cli.HTTPRequestSpec {
	if o.Type() != js.TypeObject {
		return cli.HTTPRequestSpec{}
	}
	str := func(k string) string {
		v := o.Get(k)
		if v.Type() != js.TypeString {
			return ""
		}
		return v.String()
	}
	var headers []string
	add := func(line string) {
		if strings.TrimSpace(line) != "" {
			headers = append(headers, strings.TrimSpace(line))
		}
	}
	switch h := o.Get("headers"); {
	case h.Type() == js.TypeString:
		for _, line := range strings.Split(h.String(), "\n") {
			add(line)
		}
	case h.Type() == js.TypeObject && h.Get("length").Type() == js.TypeNumber:
		for i := 0; i < h.Length(); i++ {
			if v := h.Index(i); v.Type() == js.TypeString {
				add(v.String())
			}
		}
	}
	return cli.HTTPRequestSpec{
		Method:  str("method"),
		Path:    str("path"),
		Headers: headers,
		Body:    []byte(str("body")),
	}
}

// harnessHTTPFetch sends one HTTP request over a fresh in-process forward and
// resolves with the PARSED response. Unlike rawOpen/rawSend it holds nothing:
// the forward is opened, used and closed inside this one call, so a page
// issuing N fetches leaves nothing behind to clean up.
//
// This exists for the rendered HTML preview, whose page cannot assemble
// request bytes or decode a chunked response itself — see
// docs/superpowers/specs/2026-08-12-webui-preview-pinned-api-target-design.md.
// Whether the caller is ALLOWED to reach host:port is decided by the page-side
// bridge against the target the operator pinned; this binding is the transport
// and deliberately does not re-litigate it.
//
//	harness.httpFetch(taskIDHex, host, port, {method, path, headers, body})
//	  -> Promise<{status, statusText, headers, body, truncated}>
func harnessHTTPFetch(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 4 || args[3].Type() != js.TypeObject {
				rejectErr(reject, errors.New("httpFetch: want (taskIDHex, host, port, {method, path, headers, body})"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			res, err := cli.HTTPFetch(rootCtx, c, args[0].String(), args[1].String(), args[2].Int(), httpSpecFromJS(args[3]))
			if err != nil {
				rejectErr(reject, err)
				return
			}
			body := js.Global().Get("Uint8Array").New(len(res.Body))
			js.CopyBytesToJS(body, res.Body)
			headers := js.Global().Get("Array").New(len(res.Headers))
			for i, h := range res.Headers {
				pair := js.Global().Get("Array").New(2)
				pair.SetIndex(0, h[0])
				pair.SetIndex(1, h[1])
				headers.SetIndex(i, pair)
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"status":     res.Status,
				"statusText": res.StatusText,
				"headers":    headers,
				"body":       body,
				"truncated":  res.Truncated,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harness.rawClose(key) -> Promise<void>
func harnessRawClose(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 1 {
				rejectErr(reject, errors.New("rawClose: missing pane key"))
				return
			}
			cli.CloseRawPane(args[0].String())
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
			// Wait for the live handle: on reconnect this starter is
			// re-invoked before the new client is installed (see waitForClient).
			c, err := waitForClient(rootCtx)
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

// harnessPrune asks the server to forget tasks, mirroring `harness-cli prune`'s
// two modes. With taskIds empty it runs in time mode (terminal tasks older than
// `before` are removed; force ignored). With taskIds non-empty it runs in id
// mode (before ignored; each id must be full 32-hex; active tasks are skipped
// unless force). Resolves with the cli.Prune human-readable summary text.
//
//	harness.prune({before: "168h"}) -> Promise<string>                       // time mode
//	harness.prune({taskIds: ["<32hex>", ...], force: true}) -> Promise<string> // id mode
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
			taskIDs := jsArrayToStringSlice(args[0].Get("taskIds"))
			force := args[0].Get("force").Truthy()
			// before is only consulted in time mode; skip parsing (which would
			// reject an empty/absent value) when ids were supplied.
			var before time.Duration
			if len(taskIDs) == 0 {
				before, err = time.ParseDuration(args[0].Get("before").String())
				if err != nil {
					rejectErr(reject, fmt.Errorf("invalid before duration: %w", err))
					return
				}
			}
			var buf bytesBuffer
			if err := c.Prune(rootCtx, before, taskIDs, force, &buf); err != nil {
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
// the session to a specific runner by hostname; an optional "runner" field
// pins by ConnectionID (as returned in an ambiguous_runner rejection's
// candidates — see below). BuildSelector does NOT reject supplying both —
// its switch just prefers Runner when both are set — so this relies on the
// caller (pickRunnerAndRetry in main.js) deliberately clearing host on the
// picker retry, leaving only runner set.
//
// If the selector is ambiguous (Any/ByHostname matches >=2 runners, or,
// once profile-expanded, >=2 (runner,profile) combos), the returned Promise
// rejects with a JS Error whose .code === "ambiguous_runner" and .candidates
// is an array of {cid, hostname, matchedRoot, activeTasks, maxTasks, profile};
// the caller re-invokes startInteractive with runner: candidate.cid and
// agent: candidate.profile to pin the retry (multi-agent-profile design §4a).
//
//	harness.startInteractive({repo: "/abs/path", host: "raspi", agent: "codex"}) -> Promise<taskIDHex>
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
			// Both fields are optional; opts.Get(...) returns a TypeUndefined
			// js.Value when the JS caller omits the property. runner is set by
			// the runner-picker retry and by assigned-runner resume actions.
			// js.Value.String() on a non-string type stringifies as "<TYPE>"
			// rather than "" (see syscall/js), so gate on Type() explicitly
			// instead of comparing the stringified form.
			runnerVal := opts.Get("runner")
			runnerCid := ""
			if runnerVal.Type() == js.TypeString {
				runnerCid = runnerVal.String()
			}
			hostVal := opts.Get("host")
			host := ""
			if hostVal.Type() == js.TypeString {
				host = hostVal.String()
			}
			extraArgs := jsArrayToStringSlice(opts.Get("claudeArgs"))
			sel, err := cli.BuildSelector(cli.SelectorOpts{Runner: runnerCid, Host: host})
			if err != nil {
				rejectErr(reject, fmt.Errorf("startInteractive: selector: %w", err))
				return
			}
			// Default-deny, matching ParseCaps(""): a JS caller that omits
			// `caps` spawns a task with no control plane. The WebUI always
			// sends its Compose state, so this is the floor for scripted
			// callers, not the button path.
			caps := protocol.Capability_None
			if cv := opts.Get("caps"); cv.Type() == js.TypeNumber {
				caps = protocol.Capability(uint32(cv.Int()))
			}
			scope, scopeErr := scopeFromOpts(opts)
			overrides, ovErr := overridesFromOpts(opts)
			if ovErr != nil && scopeErr == nil {
				scopeErr = ovErr
			}
			if scopeErr != nil {
				rejectErr(reject, scopeErr)
				return
			}
			resumeCapsOverride := false
			if rcov := opts.Get("resumeCapsOverride"); rcov.Type() == js.TypeBoolean {
				resumeCapsOverride = rcov.Bool()
			}
			scopePresent := false
			if sp := opts.Get("scopePresent"); sp.Type() == js.TypeBoolean {
				scopePresent = sp.Bool()
			}
			resumeConversation := false
			if rc := opts.Get("resumeConversation"); rc.Type() == js.TypeBoolean {
				resumeConversation = rc.Bool()
			}
			// agent selects a named agent profile advertised by the target
			// runner (multi-agent-profile design §6); empty = runner default
			// / (on resume) the resumed task's own profile.
			agentProfile := ""
			if av := opts.Get("agent"); av.Type() == js.TypeString {
				agentProfile = av.String()
			}
			// Initial PTY size. The WebUI itself resizes from the xterm
			// element as soon as it attaches, so these matter for scripted
			// callers opening a session they will not attach to.
			var initRows, initCols uint16
			if rv := opts.Get("rows"); rv.Type() == js.TypeNumber {
				initRows = uint16(rv.Int())
			}
			if cv := opts.Get("cols"); cv.Type() == js.TypeNumber {
				initCols = uint16(cv.Int())
			}
			var eventStream bool
			if ev := opts.Get("eventStream"); ev.Type() == js.TypeBoolean {
				eventStream = ev.Bool()
			}
			sopts := cli.SessionOpts{
				Selector: sel, ExtraArgs: extraArgs, ResumeTaskID: resumeTaskID,
				Caps: caps, Scope: scope, Overrides: overrides,
				ScopePresent: scopePresent, ResumeCapsOverride: resumeCapsOverride,
				ResumeConversation: resumeConversation, AgentProfile: agentProfile,
				InitialRows: initRows, InitialCols: initCols,
				EventStream: eventStream,
			}
			// c.Interactive itself declines to mount the xterm when
			// EventStream is set (cli/open_interactive_wasm.go): this kind has
			// no PTY, and splicing NDJSON into a terminal is what that guard
			// exists to prevent. JS attaches the chat afterwards.
			taskID, err := c.Interactive(rootCtx, repo, sopts)
			if err != nil {
				var are *cli.AmbiguousRunnerError
				if errors.As(err, &are) {
					cands := make([]any, 0, len(are.Candidates))
					for _, cc := range are.Candidates {
						cands = append(cands, map[string]any{
							"cid": cc.Cid, "hostname": cc.Hostname, "matchedRoot": cc.MatchedRoot,
							"activeTasks": cc.ActiveTasks, "maxTasks": cc.MaxTasks,
							// profile is the agent profile this (runner,profile) combo
							// represents (§4a); the picker modal shows it per-row and
							// re-issues pinned to both cid and profile on selection.
							"profile": cc.Profile,
						})
					}
					jsErr := js.Global().Get("Error").New("ambiguous_runner")
					jsErr.Set("code", "ambiguous_runner")
					jsErr.Set("candidates", js.ValueOf(cands))
					reject.Invoke(jsErr)
					return
				}
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
			mode := protocol.AttachMode_Control
			if len(args) > 1 && args[1].Type() == js.TypeString && args[1].String() == "view" {
				mode = protocol.AttachMode_View
			}
			resultID, err := c.AttachSession(rootCtx, taskIDHex, mode)
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

// harnessPreviewStart opens a LIVE preview of a detachable interactive
// session, independent of the activeInteractiveSession singleton. paneKey
// identifies which pane's JS hooks receive the output, so multiple panes can
// each hold an independent stream over the one shared client. Output flows via
// the JS hooks harness_previewOpen / harness_previewWrite /
// harness_previewResize / harness_previewClosed — each called with paneKey as
// their first argument — until harness.previewStop(paneKey) or a fresh
// previewStart for the same paneKey supersedes it.
//
// EVERY pane cowrite-attaches, so any of them can be typed into via
// harness.previewInput — non-takeover, no size authority. The optional third
// argument is not a mode: capReplay=true caps the replayed ring at
// cli.GridPaneReplayLimit, which the grid's small crops want and the full-size
// single preview does not (its scrollback is the point).
//
//	harness.previewStart(paneKey, taskIDHex, capReplay?) -> Promise<taskIDHex>
func harnessPreviewStart(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("previewStart: missing paneKey/taskIDHex arg"))
				return
			}
			paneKey := args[0].String()
			taskID := args[1].String()
			var replayLimit uint32
			if len(args) >= 3 && args[2].Truthy() {
				replayLimit = cli.GridPaneReplayLimit
			}
			if err := c.StartPreview(rootCtx, paneKey, taskID, replayLimit); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(taskID)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessPreviewInput forwards a focused pane's keystrokes to its session over
// the pane's cowrite stream — grid cell or single preview alike. No-op for an
// unknown/closed paneKey. Synchronous
// and panic-safe (a missing arg is a no-op, not a wasm-crashing args[i] panic),
// matching harnessPreviewStop.
//
//	harness.previewInput(paneKey, data)
func harnessPreviewInput(this js.Value, args []js.Value) any {
	if len(args) < 2 {
		return js.Undefined()
	}
	cli.SendPreviewInput(args[0].String(), []byte(args[1].String()))
	return js.Undefined()
}

// harnessPreviewStop tears down the named pane's live preview stream, if
// any. Synchronous and idempotent; a paused/never-started preview is a
// no-op. A missing paneKey arg is also treated as a no-op (rather than
// panicking on args[0]) since this bridge function is synchronous, not a
// promise executor — a panic here would crash the whole wasm module
// instead of just rejecting one call.
//
//	harness.previewStop(paneKey)
func harnessPreviewStop(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.Undefined()
	}
	cli.StopPreview(args[0].String())
	return js.Undefined()
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

// harnessSendNotification sends a notification to the server.
//
//	harness.sendNotification({level: "info"|"warn"|"error", title: "...", text: "..."}) -> Promise<void>
func harnessSendNotification(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("sendNotification: missing options arg"))
				return
			}
			opts := args[0]
			level := opts.Get("level").String()
			title := opts.Get("title").String()
			text := opts.Get("text").String()
			if err := c.Notify(rootCtx, level, title, text); err != nil {
				rejectErr(reject, fmt.Errorf("sendNotification: %w", err))
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessAwaitIdle arms a one-shot idle watcher on a live interactive
// session. For the default reply sink the Promise resolves when the watcher
// FIRES (potentially minutes later — callers should not await it inline in a
// UI handler unless that is the point); for notify/board sinks it resolves
// immediately with status "armed".
//
//	harness.awaitIdle({taskId: "...", thresholdMs?: N, sink?: "reply"|"notify"|"board", topic?: "..."})
//	  -> Promise<{status: string, lastOutputAt: number}>
func harnessAwaitIdle(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("awaitIdle: missing options arg"))
				return
			}
			opts := args[0]
			taskID := opts.Get("taskId").String()
			thresholdMs := uint32(0)
			if v := opts.Get("thresholdMs"); v.Type() == js.TypeNumber {
				thresholdMs = uint32(v.Int())
			}
			sink := protocol.AwaitIdleSink_Reply
			topic := ""
			switch s := opts.Get("sink"); {
			case s.Type() == js.TypeString && s.String() == "notify":
				sink = protocol.AwaitIdleSink_Notify
			case s.Type() == js.TypeString && s.String() == "board":
				sink = protocol.AwaitIdleSink_Board
				if t := opts.Get("topic"); t.Type() == js.TypeString {
					topic = t.String()
				}
			}
			// rootCtx, not a round-trip timeout: the reply sink's response is
			// deferred until the session actually goes idle.
			resp, err := c.AwaitIdle(rootCtx, taskID, thresholdMs, sink, topic)
			if err != nil {
				rejectErr(reject, fmt.Errorf("awaitIdle: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"status":       awaitIdleStatusStr(resp.Status),
				"lastOutputAt": float64(resp.LastOutputAt),
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// awaitIdleStatusStr renders the AwaitIdleStatus enum for the JS side.
func awaitIdleStatusStr(s protocol.AwaitIdleStatus) string {
	switch s {
	case protocol.AwaitIdleStatus_Fired:
		return "fired"
	case protocol.AwaitIdleStatus_Armed:
		return "armed"
	case protocol.AwaitIdleStatus_SessionStopped:
		return "session_stopped"
	case protocol.AwaitIdleStatus_NotFound:
		return "not_found"
	case protocol.AwaitIdleStatus_BadRequest:
		return "bad_request"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// harnessWatchNotifications starts a notification-watch goroutine. Events are
// pushed via window.harness_onNotifyEvent(jsonString) — one raw JSON object per
// event. The Promise resolves once the goroutine is running; the goroutine runs
// until rootCtx is cancelled or cli.WatchNotifications returns an error.
//
//	harness.watchNotifications() -> Promise<void>
func harnessWatchNotifications(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			// Wait for the live handle: on reconnect this starter is
			// re-invoked before the new client is installed (see waitForClient).
			c, err := waitForClient(rootCtx)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			pipe := &notifyPipe{}
			go func() {
				if err := c.WatchNotifications(rootCtx, pipe); err != nil {
					slog.Error("watchNotifications ended", "err", err)
				}
			}()
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// notifyPipe accumulates bytes from cli.WatchNotifications, splits on '\n',
// and forwards each complete line (already a JSON object) to
// window.harness_onNotifyEvent. Mirrors watchPipe but does NOT re-wrap the
// line — the JS side receives the raw JSON string and calls JSON.parse itself.
type notifyPipe struct {
	carry []byte
}

func (n *notifyPipe) Write(p []byte) (int, error) {
	n.carry = append(n.carry, p...)
	for {
		idx := -1
		for i, b := range n.carry {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx == -1 {
			break
		}
		line := string(n.carry[:idx])
		n.carry = n.carry[idx+1:]
		js.Global().Call("harness_onNotifyEvent", line)
	}
	return len(p), nil
}

// rejectFileErr is the file-ops sibling of rejectErr: in addition to the
// Error.message, it stamps a `code` property on the rejection so the JS
// side can branch on the underlying FileTransferStatus without having
// to string-match the error message. Recognised codes:
//
//	already_exists  – push destination present, retry with force=true
//	not_found       – source missing on the runner side
//	path_invalid    – worktree-escape attempt or empty path
//	not_a_directory – delete/dir-delete called on a wrong kind of path
//	not_empty       – dir_delete without recursive on a non-empty dir
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

// rejectFileEditErr adds the two edit-specific codes on top of
// rejectFileErr's set, so the JS side can tell "too big to open" and "not
// text" apart from a runner-side file failure without string-matching.
//
//	too_large – over cli.FileEditMaxBytes; offer Pull instead
//	not_text  – invalid UTF-8 or NUL-containing; offer Preview instead
func rejectFileEditErr(reject js.Value, err error) {
	code := ""
	switch {
	case errors.Is(err, cli.ErrFileEditTooLarge):
		code = "too_large"
	case errors.Is(err, cli.ErrFileEditNotText):
		code = "not_text"
	}
	if code == "" {
		rejectFileErr(reject, err)
		return
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

// harnessFileMkdir creates a directory at rel inside taskID's worktree.
// parents=false is strict mkdir (missing parent rejects with
// code="not_found", existing dir with code="already_exists");
// parents=true is mkdir -p (parents created, existing dir resolves).
//
//	harness.fileMkdir(taskID, rel, parents) -> Promise<void>
func harnessFileMkdir(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		if len(args) < 3 {
			rejectErr(reject, errors.New("fileMkdir: missing taskID / rel / parents args"))
			return nil
		}
		taskID := args[0].String()
		rel := args[1].String()
		parents := args[2].Truthy()
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if err := c.FileMkdir(rootCtx, taskID, rel, parents); err != nil {
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
// rejects with code="already_exists" if the destination is present.
// parents=true creates missing parent directories (mkdir -p semantics);
// false rejects with code="not_found" if the parent is missing, letting
// the JS layer drive a confirm() prompt before retrying either case.
//
//	harness.filePushBytes(taskID, remoteRel, data, force, parents[, onProgress]) -> Promise<void>
//	onProgress(transferred, total) is called ~10/s with byte counts.
//
// jsProgress adapts an optional JS progress callback at args[idx] into a
// cli.ProgressFunc forwarding (transferred, total) as JS numbers (total 0 =
// unknown). Returns nil when no function is supplied, so the transfer runs
// without reporting. cli throttles these to ~10/s, so forwarding straight into
// JS is safe for the single event loop.
func jsProgress(args []js.Value, idx int) cli.ProgressFunc {
	if len(args) <= idx || args[idx].Type() != js.TypeFunction {
		return nil
	}
	cb := args[idx]
	return func(transferred, total uint64) {
		cb.Invoke(float64(transferred), float64(total))
	}
}

func harnessFilePushBytes(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		// Copy bytes out of the Uint8Array on the JS heap into a Go
		// []byte while we're still on the main thread; the goroutine
		// below cannot reach JS values directly.
		if len(args) < 5 {
			rejectErr(reject, errors.New("filePushBytes: missing taskID / remoteRel / data / force / parents args"))
			return nil
		}
		taskID := args[0].String()
		remoteRel := args[1].String()
		dataJS := args[2]
		force := args[3].Truthy()
		parents := args[4].Truthy()
		length := dataJS.Length()
		data := make([]byte, length)
		js.CopyBytesToGo(data, dataJS)
		onProgress := jsProgress(args, 5)
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if err := c.FilePushBytes(rootCtx, taskID, data, remoteRel, cli.FilePushOpts{Force: force, MkdirParents: parents}, onProgress); err != nil {
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
//	harness.filePullBytes(taskID, remoteRel[, onProgress]) -> Promise<Uint8Array>
//	onProgress(transferred, total) is called ~10/s; total is the file size.
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
			data, err := c.FilePullBytes(rootCtx, taskID, remoteRel, jsProgress(args, 2))
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

// harnessPreviewPinOpen registers the reach a pinned preview was granted and
// holds it for the preview's life, resolving with the forward id.
//
// The registration is the point: a fetch-scoped one lived tens of milliseconds
// against a registry with no history, so an operator could never see what a
// previewed page was reaching. This row stays up while the preview does, and
// killing it from any surface revokes the reach.
//
//	harness.previewPinOpen(key, taskIDHex, host, port) -> Promise<number>
func harnessPreviewPinOpen(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 4 {
				rejectErr(reject, errors.New("previewPinOpen: want (key, taskIDHex, host, port)"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			fid, err := cli.OpenPreviewPin(rootCtx, c, args[0].String(), args[1].String(), args[2].String(), args[3].Int())
			if err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.ValueOf(fid))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessPreviewPinFetch sends one request over a connection opened under the
// pin. The connection is not separately registered — the pin already stands for
// it, for longer and more visibly than a per-request row.
//
//	harness.previewPinFetch(key, {method, path, headers, body})
//	  -> Promise<{status, statusText, headers, body, truncated}>
func harnessPreviewPinFetch(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 2 || args[1].Type() != js.TypeObject {
				rejectErr(reject, errors.New("previewPinFetch: want (key, {method, path, headers, body})"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			res, err := cli.PreviewPinFetch(rootCtx, c, args[0].String(), httpSpecFromJS(args[1]))
			if err != nil {
				rejectErr(reject, err)
				return
			}
			body := js.Global().Get("Uint8Array").New(len(res.Body))
			js.CopyBytesToJS(body, res.Body)
			headers := js.Global().Get("Array").New(len(res.Headers))
			for i, h := range res.Headers {
				pair := js.Global().Get("Array").New(2)
				pair.SetIndex(0, h[0])
				pair.SetIndex(1, h[1])
				headers.SetIndex(i, pair)
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"status":     res.Status,
				"statusText": res.StatusText,
				"headers":    headers,
				"body":       body,
				"truncated":  res.Truncated,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harness.previewPinClose(key) -> Promise<void>
func harnessPreviewPinClose(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 1 {
				rejectErr(reject, errors.New("previewPinClose: missing key"))
				return
			}
			cli.ClosePreviewPin(args[0].String())
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFilePullBytesRange fetches one byte range of a file and resolves with
// the slice plus the size of the whole file.
//
// total is the reason this is not just filePullBytes with two more arguments:
// a caller rendering a head has to know whether it truncated, and asking
// separately with fileLs races the pull.
//
//	harness.filePullBytesRange(taskID, remoteRel, offset, length[, onProgress])
//	  -> Promise<{bytes: Uint8Array, total: number}>
//	length 0 = to end of file.
func harnessFilePullBytesRange(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 4 {
				rejectErr(reject, errors.New("filePullBytesRange: want (taskID, remoteRel, offset, length[, onProgress])"))
				return
			}
			rng := cli.FileTransferRange{
				Offset: uint64(args[2].Int()),
				Length: uint64(args[3].Int()),
			}
			data, total, err := c.FilePullBytesRange(rootCtx, args[0].String(), args[1].String(), rng, jsProgress(args, 4))
			if err != nil {
				rejectFileErr(reject, err)
				return
			}
			out := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(out, data)
			resolve.Invoke(js.ValueOf(map[string]any{"bytes": out, "total": total}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFilePullDirBytes fetches the directory at remoteRel from taskID's
// worktree and resolves with a Uint8Array holding a tar archive of the
// tree. The JS layer wraps it in a Blob and triggers a "<name>.tar"
// download (the runner streams tar regardless of host OS; the user
// extracts locally — Windows 11 / `tar -xf` handle it).
//
//	harness.filePullDirBytes(taskID, remoteRel[, onProgress]) -> Promise<Uint8Array>
//	onProgress(transferred, 0) is called ~10/s; total is 0 (tar size unknown).
func harnessFilePullDirBytes(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("filePullDirBytes: missing taskID / remoteRel args"))
				return
			}
			taskID := args[0].String()
			remoteRel := args[1].String()
			data, err := c.FilePullDirBytes(rootCtx, taskID, remoteRel, jsProgress(args, 2))
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

// harnessFileEditLoad pulls remoteRel from taskID's worktree and resolves
// with its editable form: the text an editor widget shows, the exact bytes
// pulled (the caller hands them back on commit as the conflict baseline),
// and the BOM / CRLF flags needed to reassemble the file.
//
//	harness.fileEditLoad(taskID, remoteRel[, onProgress])
//	    -> Promise<{text, orig: Uint8Array, crlf, bom}>
//
// Rejects with code "too_large" / "not_text" for files an editor should not
// open; runner-side failures keep the codes rejectFileErr already assigns.
func harnessFileEditLoad(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		if len(args) < 2 {
			rejectErr(reject, errors.New("fileEditLoad: missing taskID / remoteRel args"))
			return nil
		}
		taskID := args[0].String()
		remoteRel := args[1].String()
		onProgress := jsProgress(args, 2)
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			doc, err := c.FileEditLoad(rootCtx, taskID, remoteRel, onProgress)
			if err != nil {
				rejectFileEditErr(reject, err)
				return
			}
			orig := js.Global().Get("Uint8Array").New(len(doc.Orig))
			js.CopyBytesToJS(orig, doc.Orig)
			resolve.Invoke(map[string]any{
				"text": doc.Text,
				"orig": orig,
				"crlf": doc.CRLF,
				"bom":  doc.BOM,
			})
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFileEditCommit writes text back to remoteRel. orig is what
// fileEditLoad returned; the commit re-reads the runner-side file and
// refuses to overwrite one that moved since, unless force is set.
//
//	harness.fileEditCommit(taskID, remoteRel, orig, text, crlf, bom, force)
//	    -> Promise<{status: "pushed" | "unchanged" | "conflict"}>
func harnessFileEditCommit(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		// Read every JS value here, on the main thread: args is invalid once
		// this callback returns, and the goroutine below outlives it.
		if len(args) < 7 {
			rejectErr(reject, errors.New("fileEditCommit: missing taskID / remoteRel / orig / text / crlf / bom / force args"))
			return nil
		}
		taskID := args[0].String()
		remoteRel := args[1].String()
		origJS := args[2]
		orig := make([]byte, origJS.Length())
		js.CopyBytesToGo(orig, origJS)
		text := args[3].String()
		doc := cli.FileEditDoc{Rel: remoteRel, Orig: orig, CRLF: args[4].Truthy(), BOM: args[5].Truthy()}
		force := args[6].Truthy()
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			st, err := c.FileEditCommit(rootCtx, taskID, doc, text, force)
			if err != nil {
				rejectFileEditErr(reject, err)
				return
			}
			resolve.Invoke(map[string]any{"status": st.String()})
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessFileEditEncode applies the BOM / CRLF rules to text without
// touching the network, so the save-as path can hand bytes to filePushBytes
// (and its already_exists / not_found prompts) without reimplementing those
// rules in JavaScript. Synchronous: there is nothing to await.
//
//	harness.fileEditEncode(text, crlf, bom) -> Uint8Array
func harnessFileEditEncode(this js.Value, args []js.Value) any {
	if len(args) < 3 {
		return js.Global().Get("Uint8Array").New(0)
	}
	doc := cli.FileEditDoc{CRLF: args[1].Truthy(), BOM: args[2].Truthy()}
	data := doc.Encode(args[0].String())
	out := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(out, data)
	return out
}

// gitKindFromJS maps the page's kind string onto the protocol enum. An
// unknown string is an explicit error rather than a silent zero value, which
// would quietly turn a typo into a log query.
func gitKindFromJS(s string) (protocol.GitQueryKind, error) {
	switch s {
	case "log":
		return protocol.GitQueryKind_Log, nil
	case "diff":
		return protocol.GitQueryKind_Diff, nil
	case "show":
		return protocol.GitQueryKind_Show, nil
	case "status":
		return protocol.GitQueryKind_Status, nil
	case "subrepos":
		return protocol.GitQueryKind_Subrepos, nil
	case "file":
		return protocol.GitQueryKind_File, nil
	}
	return 0, fmt.Errorf("gitQuery: unknown kind %q", s)
}

func gitTargetFromJS(s string) (protocol.GitDiffTarget, error) {
	switch s {
	case "", "worktree":
		return protocol.GitDiffTarget_Worktree, nil
	case "index":
		return protocol.GitDiffTarget_Index, nil
	case "rev":
		return protocol.GitDiffTarget_Rev, nil
	}
	return 0, fmt.Errorf("gitQuery: unknown target %q", s)
}

// optString reads a string property from an options object, tolerating an
// absent or non-object options argument.
func optString(opts js.Value, key string) string {
	if opts.Type() != js.TypeObject {
		return ""
	}
	v := opts.Get(key)
	if v.Type() != js.TypeString {
		return ""
	}
	return v.String()
}

func optBool(opts js.Value, key string) bool {
	if opts.Type() != js.TypeObject {
		return false
	}
	return opts.Get(key).Truthy()
}

func optUint32(opts js.Value, key string) uint32 {
	if opts.Type() != js.TypeObject {
		return 0
	}
	v := opts.Get(key)
	if v.Type() != js.TypeNumber {
		return 0
	}
	n := v.Float()
	if n < 0 {
		return 0
	}
	return uint32(n)
}

// harnessGitQuery runs one read-only git query against a task's worktree.
//
//	harness.gitQuery(taskID, kind, {target, baseRev, targetRev, path, subrepo,
//	                                submodule, maxCommits, maxBytes})
//
// kind is "log" | "diff" | "show" | "status" | "subrepos" | "file"; target is
// "worktree" | "index" | "rev". subrepo re-roots the whole query into a nested
// repository; path only filters within whichever repository that is. A non-ok runner status RESOLVES rather than rejecting — it is git's
// answer, not a transport failure — with status and stderr set, so the page can
// render git's own words where the diff would have gone.
func harnessGitQuery(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("gitQuery: missing taskID / kind args"))
				return
			}
			taskID := args[0].String()
			kind, err := gitKindFromJS(args[1].String())
			if err != nil {
				rejectErr(reject, err)
				return
			}
			var opts js.Value
			if len(args) > 2 {
				opts = args[2]
			} else {
				opts = js.Undefined()
			}
			target, err := gitTargetFromJS(optString(opts, "target"))
			if err != nil {
				rejectErr(reject, err)
				return
			}
			res, err := c.GitQuery(rootCtx, taskID, cli.GitQuery{
				Kind:          kind,
				Target:        target,
				BaseRev:       optString(opts, "baseRev"),
				TargetRev:     optString(opts, "targetRev"),
				Path:          optString(opts, "path"),
				Subrepo:       optString(opts, "subrepo"),
				SubmoduleDiff: optBool(opts, "submodule"),
				MaxCommits:    optUint32(opts, "maxCommits"),
				MaxBytes:      optUint32(opts, "maxBytes"),
			})
			if err != nil {
				rejectErr(reject, err)
				return
			}
			commits := make([]any, 0, len(res.Commits))
			for _, cm := range res.Commits {
				commits = append(commits, map[string]any{
					"sha":     cm.SHA,
					"short":   cm.Short(),
					"author":  cm.Author,
					"when":    float64(cm.When.Unix()),
					"subject": cm.Subject,
				})
			}
			entries := make([]any, 0, len(res.Entries))
			for _, e := range res.Entries {
				entries = append(entries, map[string]any{"xy": e.XY, "path": e.Path})
			}
			subrepos := make([]any, 0, len(res.Subrepos))
			for _, sr := range res.Subrepos {
				subrepos = append(subrepos, sr)
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"status":    res.Status.String(),
				"ok":        res.Status == protocol.GitRunStatus_Ok,
				"stderr":    res.Stderr,
				"kind":      args[1].String(),
				"text":      res.Text,
				"truncated": res.Truncated,
				"commits":   commits,
				"entries":   entries,
				"subrepos":  subrepos,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// --- event-stream chat -----------------------------------------------------
//
// The WebUI's driving surface for TaskKind_Stream. Reading is a pump into JS
// hooks (cli/streamchat_wasm.go); WRITING goes through cli's message builders
// so the browser never assembles an adapter-protocol line itself — the same
// rule the CLI and the TUI follow, and the reason all three cannot drift.

// harnessStreamStart cowrite-attaches a stream task and starts the pump.
//
//	await harness.streamStart(taskIDHex)
func harnessStreamStart(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("streamStart: missing taskIDHex arg"))
				return
			}
			taskID := args[0].String()
			if err := c.StartStreamChat(rootCtx, taskID); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(taskID)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessStreamStop closes the chat attach. Synchronous and idempotent, like
// harnessPreviewStop — a teardown that returns a promise invites a close that
// has not finished when the next open starts.
//
//	harness.streamStop()
func harnessStreamStop(this js.Value, args []js.Value) any {
	cli.StopStreamChat()
	return js.Undefined()
}

// streamWrite is the shared body of the four write verbs: build the message in
// cli, hand it to the held attach, and settle the promise.
func streamWrite(args []js.Value, build func(args []js.Value) (string, streamagent.Msg, error)) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			taskID, msg, err := build(args)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if err := cli.SendStreamChat(taskID, msg); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// await harness.streamTurn(taskIDHex, text)
func harnessStreamTurn(this js.Value, args []js.Value) any {
	return streamWrite(args, func(a []js.Value) (string, streamagent.Msg, error) {
		if len(a) < 2 {
			return "", streamagent.Msg{}, errors.New("streamTurn: want (taskIDHex, text)")
		}
		return a[0].String(), streamagent.Msg{
			Kind: streamagent.KindUser,
			User: &streamagent.UserTurn{Text: a[1].String()},
		}, nil
	})
}

// harnessStreamApprove answers a pending request.
//
// suggestion is -1 for "no suggestion"; JS passes a number, and a missing or
// negative one means the operator answered only this call. The deny message
// reaches the AGENT verbatim as a failed tool result.
//
//	await harness.streamApprove(taskIDHex, requestID, "allow"|"deny", message, suggestionIndex)
func harnessStreamApprove(this js.Value, args []js.Value) any {
	return streamWrite(args, func(a []js.Value) (string, streamagent.Msg, error) {
		if len(a) < 3 {
			return "", streamagent.Msg{}, errors.New("streamApprove: want (taskIDHex, requestID, behavior[, message, suggestion])")
		}
		reqID := a[1].String()
		if reqID == "" {
			return "", streamagent.Msg{}, errors.New("approve: a request id is required — it is what makes a stale answer a refusal rather than a misapplied one")
		}
		resp := streamagent.Response{ID: reqID}
		switch a[2].String() {
		case "allow":
			resp.Behavior = streamagent.BehaviorAllow
		case "deny":
			resp.Behavior = streamagent.BehaviorDeny
			if len(a) > 3 && a[3].Truthy() {
				resp.Message = a[3].String()
			}
		default:
			return "", streamagent.Msg{}, fmt.Errorf("approve: behavior must be allow or deny, got %q", a[2].String())
		}
		if len(a) > 4 && a[4].Type() == js.TypeNumber {
			if n := a[4].Int(); n >= 0 {
				resp.AcceptSuggestion = &n
			}
		}
		return a[0].String(), streamagent.Msg{Kind: streamagent.KindResponse, Response: &resp}, nil
	})
}

// await harness.streamInterrupt(taskIDHex)
func harnessStreamInterrupt(this js.Value, args []js.Value) any {
	return streamWrite(args, func(a []js.Value) (string, streamagent.Msg, error) {
		if len(a) < 1 {
			return "", streamagent.Msg{}, errors.New("streamInterrupt: want (taskIDHex)")
		}
		return a[0].String(), streamagent.Msg{
			Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{},
		}, nil
	})
}

// await harness.streamFinish(taskIDHex)
func harnessStreamFinish(this js.Value, args []js.Value) any {
	return streamWrite(args, func(a []js.Value) (string, streamagent.Msg, error) {
		if len(a) < 1 {
			return "", streamagent.Msg{}, errors.New("streamFinish: want (taskIDHex)")
		}
		return a[0].String(), streamagent.Msg{
			Kind: streamagent.KindFinish, Finish: &streamagent.Finish{},
		}, nil
	})
}
