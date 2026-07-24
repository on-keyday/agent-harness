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
		"previewStart":       js.FuncOf(harnessPreviewStart),
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
		"filePullDirBytes":   js.FuncOf(harnessFilePullDirBytes),
		"serverDialRunner":   js.FuncOf(harnessServerDialRunner),
		"sendNotification":   js.FuncOf(harnessSendNotification),
		"awaitIdle":          js.FuncOf(harnessAwaitIdle),
		"watchNotifications": js.FuncOf(harnessWatchNotifications),
		"capList":            js.FuncOf(harnessCapList),
		"boardTopics":        js.FuncOf(harnessBoardTopics),
		"boardRead":          js.FuncOf(harnessBoardRead),
		"boardPurge":         js.FuncOf(harnessBoardPurge),
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
			caps := protocol.Capability_All
			if cv := opts.Get("caps"); cv.Type() == js.TypeNumber {
				caps = protocol.Capability(uint32(cv.Int()))
			}
			resumeCapsOverride := false
			if rcov := opts.Get("resumeCapsOverride"); rcov.Type() == js.TypeBoolean {
				resumeCapsOverride = rcov.Bool()
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
				Caps: cli.CapsPtr(caps), ResumeCapsOverride: resumeCapsOverride,
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
//	  runners: [{hostname, status, tasks, maxTasks, roots, connectedAt, lastSeen, agentBin, agentProfiles, skillsInjected}],
//	  tasks:   [{id, status, kind, repoPath, prompt, assignedTo, exitCode,
//	             createdAt, startedAt, endedAt, agentProfile, errorMsg}],
//	  conns:   [{cid, role, remoteAddr, principalTask, connectedAt, identified}]
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
					"caps":       cli.CapsLabel(t.Capabilities),
					// agentProfile is the named profile this task last ran under
					// (empty = runner default); the resume action sheet's agent
					// dropdown defaults to this (multi-agent-profile design §4b).
					"agentProfile": string(t.AgentProfile),
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
			resolve.Invoke(js.ValueOf(map[string]any{
				"runners": runners,
				"tasks":   tasks,
				"conns":   conns,
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

// harnessBoardTopics lists all agentboard topics with aggregate metadata.
//
//	harness.boardTopics() -> Promise<[{name, lastSeq, lastPublishedAtMs, msgCount}]>
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
//	harness.boardRead(topic) -> Promise<{found, msgs:[{seq,fromTask,fromHostname,receivedAtMs,payload}]}>
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
			msgsOut := make([]any, 0, len(msgs))
			for _, m := range msgs {
				msgsOut = append(msgsOut, map[string]any{
					// Decimal string, not float64: board seq exceeds JS's
					// 2^53 safe-integer range and would round, so a purge
					// keyed on the rounded seq never matches server-side.
					"seq":          strconv.FormatUint(m.Seq, 10),
					"fromTask":     m.FromTaskHex,
					"fromHostname": m.FromHostname,
					"receivedAtMs": float64(m.ReceivedAtMs),
					"payload":      string(m.Payload),
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
			caps := protocol.Capability_All
			if cv := opts.Get("caps"); cv.Type() == js.TypeNumber {
				caps = protocol.Capability(uint32(cv.Int()))
			}
			resumeCapsOverride := false
			if rcov := opts.Get("resumeCapsOverride"); rcov.Type() == js.TypeBoolean {
				resumeCapsOverride = rcov.Bool()
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
			taskID, err := c.Interactive(rootCtx, repo, cli.SessionOpts{
				Selector: sel, ExtraArgs: extraArgs, ResumeTaskID: resumeTaskID,
				Caps: cli.CapsPtr(caps), ResumeCapsOverride: resumeCapsOverride,
				ResumeConversation: resumeConversation, AgentProfile: agentProfile,
			})
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
// The optional third argument selects the attach mode: cowrite=true
// (AttachMode_Cowrite) lets the pane forward keystrokes via
// harness.previewInput (the session grid's per-cell typing); cowrite falsey
// (AttachMode_View) is strictly read-only (the single session preview). Both
// are non-takeover and claim no size authority.
//
//	harness.previewStart(paneKey, taskIDHex, cowrite?) -> Promise<taskIDHex>
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
			cowrite := len(args) >= 3 && args[2].Truthy()
			if err := c.StartPreview(rootCtx, paneKey, taskID, cowrite); err != nil {
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

// harnessPreviewInput forwards a focused grid pane's keystrokes to its session
// over the pane's cowrite stream. No-op for a read-only (view) pane — the
// server discards a viewer's input — or an unknown/closed paneKey. Synchronous
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
