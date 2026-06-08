# notify (live-leg consumers) Implementation Plan — Plan B

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use `- [ ]` checkboxes.
>
> **Project preconditions for EVERY subagent (see `.claude/skills/implementation-pitfalls/SKILL.md`):**
> - Work in the worktree `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/0f0d4dd6b7d3b64354cf4ff249b87403/`. Paths under the parent `/home/kforfk/workspace/remote-agent-harness/<rel>` route to a DIFFERENT checkout — always use the full worktree prefix. Verify `pwd` + branch `harness/0f0d4dd6b7d3b64354cf4ff249b87403` before writing.
> - Read `.claude/skills/implementation-pitfalls/SKILL.md` first. Note: **`peer.Conn.Close` vs `Connection().Close`**, **layer-sibling grep before extending TUI/WebUI**, **reuse the long-lived `*cli.Client` in TUI/WebUI** (call `(*Client).Notify` / new `*With` helpers, never a fresh dial), **build hygiene** (`go build ./...` / `make check`; never bare `go build ./cmd/<x>/`), and **pubsub fragility** (a prior wedge came from blocking the recv/accept-queue path — the replay writes only to the new subscriber's SEND stream, never the recv path).
> - **Edit canonical sources, not runner-injected copies.** The harness-cli skill's canonical source is `runner/agentskills/harness-cli/SKILL.md`; the worktree's `.claude/skills/` + `.agents/skills/` are injected copies — never commit them.
> - `git add` only this task's files; never `git add -A` (the worktree carries unrelated pre-existing uncommitted changes).

**Goal:** Make notifications observable live: a server-side ring **replay-on-subscribe** over the `notifications` pubsub topic, a cli watch helper, TUI display + send, WebUI display + send, and E2E coverage of both legs.

**Architecture:** Reuse the existing pubsub fan-out (`tasks.status`-style) and the established TUI log-subscription / WebUI wasm-watch patterns. The only genuinely new server mechanism is a pubsub `OnSubscribe` hook that flushes `notifyRing.snapshot()` to a newly-joined `notifications` subscriber (inside the broker lock, so backlog precedes any live event). Plan A already produces the events (ring + topic publish); Plan B consumes them.

**Tech Stack:** Go; bubbletea (TUI); Go/wasm + vanilla JS (WebUI); `integration/` E2E harness.

---

## File structure

| File | Responsibility | Task |
|------|----------------|------|
| `pubsub/pubsub.go` | add `OnSubscribe` hook field; call it in `Subscribe` (in-lock, post-AddSubscriber) | 1 |
| `pubsub/pubsub_test.go` | OnSubscribe fires on join with the new subscriber's stream | 1 |
| `server/server.go` (`New`) | wire `pubsub.OnSubscribe` → replay `notifyRing.snapshot()` to `notifications` joiners | 2 |
| `cli/notify_watch.go` (new) | `(*Client).WatchNotifications(ctx, out)` + `drainNotifyEvents` + `formatNotifyEvent` | 3 |
| `cli/notify_watch_test.go` (new) | `drainNotifyEvents` decodes multiple events / partial buffers | 3 |
| `server/notify_replay_test.go` (new) | unit: ring → OnSubscribe replay writes encoded events to the stream | 2 |
| `integration/notify_e2e_test.go` (new) | E2E: egress hook fires; live subscriber gets ring backlog + live event | 4 |
| `tui/events.go` | `NotifyEventMsg` + `SubscribeNotifications` (mirror `SubscribeTaskLog`) | 5 |
| `tui/notify.go` (new) | `NotifyModel` pane (buffer + render recent events) | 5 |
| `tui/app.go` | focus/route the pane; handle `NotifyEventMsg`; start the subscription; `DoNotify` send + cmdline `notify` | 5 |
| `cmd/harness-webui-wasm/main.go` | `sendNotification` + `watchNotifications` wasm exports | 6 |
| `webui/static/main.js` | `harness_onNotifyEvent` consumer + send-form handler | 6 |
| `webui/index.html` | notifications list + send form | 6 |

---

### Task 1: pubsub `OnSubscribe` replay hook

**Files:** Modify `pubsub/pubsub.go`; Test `pubsub/pubsub_test.go`.

- [ ] **Step 1: Read context.** Read `pubsub/pubsub.go` — the `PubSub` struct (fields `m sync.Mutex`, `topics`, `taps`, `logger`), the `Subscribe(requestID, topic, nickName, sub)` function, and the `Publish` function (to see how data is written to a subscriber stream: `stream.conn.AppendData(false, msg)` where the stream is the bidi stream created in `Subscribe`). Confirm the bidi stream type returned by `sub.transport.CreateBidirectionalStream()` and that it has `AppendData(bool, []byte)` (grep the `trsf` interface).

- [ ] **Step 2: Write the failing test** — append to `pubsub/pubsub_test.go` (read the file first to match its existing test harness / how it builds a `PubSub` + a `Subscriber` with a transport; mirror an existing subscribe test). The test must assert: after setting `ps.OnSubscribe`, a `Subscribe(...)` to topic "T" invokes the hook exactly once with `topic == "T"` and a non-nil stream. Use the test harness already present in that file. Sketch:

```go
func TestPubSub_OnSubscribeHookFires(t *testing.T) {
	ps := /* construct as existing tests do */
	var gotTopic string
	var calls int
	ps.OnSubscribe = func(topic string, stream trsf.BidirectionalStream) {
		gotTopic = topic
		calls++
	}
	sub := /* construct a Subscriber with a transport, as existing tests do */
	ps.Subscribe(1, "T", "nick", sub)
	if calls != 1 || gotTopic != "T" {
		t.Fatalf("OnSubscribe calls=%d topic=%q, want 1 / T", calls, gotTopic)
	}
}
```
(If the existing tests use a mock transport, reuse it. If constructing a real trsf transport is heavy, follow whatever the existing subscribe-path tests in this file already do — do NOT invent a new harness.)

Run the test → FAIL (`OnSubscribe` field undefined).

- [ ] **Step 3: Implement.** In `pubsub/pubsub.go`:
  - Add a field to the `PubSub` struct:
    ```go
    // OnSubscribe, when non-nil, is called once per successful Subscribe, with
    // the joined topic and the new subscriber's send stream, while the broker
    // lock is held — so anything written here reaches the subscriber BEFORE any
    // concurrent Publish to the same topic. Used for replay-on-subscribe
    // (notifications ring backlog). Writes go only to this one stream's send
    // side; it must not read/block on the receive path.
    OnSubscribe func(topic string, stream trsf.BidirectionalStream)
    ```
  - In `Subscribe`, AFTER `ps.topics[topic].AddSubscriber(sub)` and BEFORE the `return &protocol.PubSubResponse{...}`, add (still inside the held `ps.m` lock):
    ```go
    if ps.OnSubscribe != nil {
        ps.OnSubscribe(topic, stream)
    }
    ```
  (`stream` is the local variable already created by `sub.transport.CreateBidirectionalStream()`.)

  Run the test → PASS.

- [ ] **Step 4: Build + commit.** `go build ./...` clean; `go test ./pubsub/ -run TestPubSub_OnSubscribe -v` pass.
```bash
git add pubsub/pubsub.go pubsub/pubsub_test.go
git commit -m "feat(pubsub): OnSubscribe hook for replay-on-join (in-lock, send-only)"
```

---

### Task 2: server replay wiring + unit test

**Files:** Modify `server/server.go` (`New`); Test `server/notify_replay_test.go`.

- [ ] **Step 1: Write the failing test** — create `server/notify_replay_test.go`. It verifies the replay closure (the function the server assigns to `pubsub.OnSubscribe`) writes the ring's encoded events to the provided stream when (and only when) the topic is `notifications`. To avoid a full pubsub/transport, test the closure in isolation by extracting it as a named method/function. So FIRST decide the shape: implement `func replayNotifications(ring *notifyRing, topic string, stream notifyStreamWriter)` where `notifyStreamWriter` is a tiny interface `interface{ AppendData(bool, []byte) (…) }` matching `trsf.BidirectionalStream`'s `AppendData` (grep its exact signature/return). Test with a fake writer capturing payloads:

```go
type fakeStream struct{ writes [][]byte }
func (f *fakeStream) AppendData(fin bool, b []byte) (/* match real sig */) {
	f.writes = append(f.writes, append([]byte(nil), b...))
	/* return zero values matching the real signature */
}

func TestReplayNotifications_OnlyNotificationsTopic(t *testing.T) {
	r := newNotifyRing(8)
	r.append(protocol.NotifyEvent{Ts: 1, TextLen: 1, Text: []byte("a")})
	r.append(protocol.NotifyEvent{Ts: 2, TextLen: 1, Text: []byte("b")})

	// wrong topic → no writes
	var other fakeStream
	replayNotifications(r, "tasks.status", &other)
	if len(other.writes) != 0 {
		t.Fatalf("replayed to wrong topic: %d writes", len(other.writes))
	}
	// notifications topic → one write per ring entry, decodable, in order
	var nf fakeStream
	replayNotifications(r, topics.Notifications(), &nf)
	if len(nf.writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(nf.writes))
	}
	var ev protocol.NotifyEvent
	if _, err := ev.Decode(nf.writes[0]); err != nil || ev.Ts != 1 {
		t.Fatalf("first replayed event wrong: ts=%d err=%v", ev.Ts, err)
	}
}
```
Run → FAIL (`replayNotifications` / interface undefined).

- [ ] **Step 2: Implement `replayNotifications`** in `server/server.go` (or a small `server/notify_replay.go`):
```go
// notifyStreamWriter is the subset of trsf.BidirectionalStream that replay needs.
type notifyStreamWriter interface {
	AppendData(fin bool, b []byte) (/* exact return types from trsf.BidirectionalStream.AppendData */)
}

// replayNotifications writes the ring backlog (oldest first) to a newly-joined
// subscriber of the notifications topic. No-op for any other topic. Send-only.
func replayNotifications(ring *notifyRing, topic string, stream notifyStreamWriter) {
	if topic != topics.Notifications() {
		return
	}
	for _, ev := range ring.snapshot() {
		stream.AppendData(false, ev.MustAppend(nil))
	}
}
```
(Match `AppendData`'s real signature exactly — grep `func (.*) AppendData(` in the trsf package for the concrete return types, and make the interface match so `trsf.BidirectionalStream` satisfies it.)

- [ ] **Step 3: Wire into the server.** In `server/server.go` `New`, after the existing `s.taskHandler.OnNotify = ...` block, add:
```go
s.pubsub.OnSubscribe = func(topic string, stream trsf.BidirectionalStream) {
	replayNotifications(s.notifyRing, topic, stream)
}
```
(Confirm `trsf` is imported in server.go — `NewStreams` is used there, so it is.)

- [ ] **Step 4: Build + test + commit.** `go build ./...`; `go test ./server/ -run 'TestReplayNotifications|TestRunNotifyHook|TestNotifyRing|TestHandleNotify' -v` pass; `go test ./...` green.
```bash
git add server/server.go server/notify_replay.go server/notify_replay_test.go
git commit -m "feat(server): replay notify ring to new notifications subscribers"
```

---

### Task 3: cli `WatchNotifications` helper

**Files:** Create `cli/notify_watch.go`, `cli/notify_watch_test.go`.

- [ ] **Step 1: Read context.** Read `cli/watch.go` in full — mirror `Watch()` (single topic this time) and the `drainTaskEvents` decode-advance pattern (loop `ev.Decode(buf)`; on success advance `buf = rest` and emit; on error break to await more bytes).

- [ ] **Step 2: Write the failing test** — create `cli/notify_watch_test.go`. Test `drainNotifyEvents` decodes a buffer holding TWO concatenated encoded `NotifyEvent`s, writes two lines, and returns the leftover (a partial third event's bytes) undrained:

```go
func TestDrainNotifyEvents(t *testing.T) {
	e1 := (&protocol.NotifyEvent{Ts: 1, Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_External, TextLen: 2, Text: []byte("hi")}).MustAppend(nil)
	e2 := (&protocol.NotifyEvent{Ts: 2, Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("x")}).MustAppend(nil)
	buf := append(append([]byte{}, e1...), e2...)
	buf = append(buf, e2[:1]...) // partial third

	var out bytes.Buffer
	var mu sync.Mutex
	rest := drainNotifyEvents(buf, &out, &mu)

	if n := strings.Count(out.String(), "\n"); n != 2 {
		t.Fatalf("emitted %d lines, want 2:\n%s", n, out.String())
	}
	if len(rest) != 1 {
		t.Fatalf("leftover = %d bytes, want 1 (the partial event)", len(rest))
	}
}
```
Run → FAIL.

- [ ] **Step 3: Implement `cli/notify_watch.go`:**
```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// WatchNotifications subscribes to the notifications topic and writes one JSON
// object per line to out for each NotifyEvent (backlog replay first, then live).
// Method form: callable on an existing *Client (TUI/WebUI reuse their client).
func (c *Client) WatchNotifications(ctx context.Context, out io.Writer) error {
	topic := topics.Notifications()
	stream, err := c.Peer().JoinAndGetStream(ctx, "notify-watch", topic)
	if err != nil {
		return fmt.Errorf("join %s: %w", topic, err)
	}
	var mu sync.Mutex
	go func() {
		var buf []byte
		for {
			data, eof, err := stream.ReadDirect(4096)
			if err != nil {
				return
			}
			if len(data) > 0 {
				buf = append(buf, data...)
				buf = drainNotifyEvents(buf, out, &mu)
			}
			if eof {
				return
			}
		}
	}()
	<-ctx.Done()
	return ctx.Err()
}

// drainNotifyEvents decodes as many whole NotifyEvents as buf holds, writing one
// JSON line each, and returns the undrained remainder.
func drainNotifyEvents(buf []byte, out io.Writer, mu *sync.Mutex) []byte {
	for {
		ev := &protocol.NotifyEvent{}
		rest, err := ev.Decode(buf)
		if err != nil {
			break // incomplete; await more bytes
		}
		mu.Lock()
		line, _ := json.Marshal(notifyEventJSON(ev))
		fmt.Fprintf(out, "%s\n", line)
		mu.Unlock()
		buf = rest
	}
	return buf
}

// notifyEventJSON is the line shape consumed by the WebUI / any line reader.
func notifyEventJSON(ev *protocol.NotifyEvent) map[string]any {
	m := map[string]any{
		"ts":     ev.Ts,
		"level":  ev.Level.String(),
		"origin": ev.Origin.String(),
		"title":  string(ev.Title),
		"text":   string(ev.Text),
	}
	if w := ev.Worker(); w != nil {
		m["task_id"] = string(w.TaskId)
		m["runner_id"] = string(w.RunnerId)
		m["repo"] = string(w.Repo)
		m["hostname"] = string(w.Hostname)
	}
	return m
}

// WatchNotifications (package-level): dial + watch + close. For short-lived CLI.
func WatchNotifications(ctx context.Context, peerCID /* objproto.ConnectionID */ any, out io.Writer) error {
	return errUseMethodForm // placeholder — see Step 4
}
```
NOTE: drop the broken package-level stub; the free function must mirror `cli/watch.go`'s style. Read how `watch.go` exposes its free/method forms and follow it exactly (the method form above is the one TUI/WebUI use; add a free `WatchNotifications(ctx, peerCID, out)` that `Dial`s + calls the method + closes, only if `cli/watch.go` has an equivalent free form — otherwise omit it). Confirm `ev.Level.String()`/`ev.Origin.String()` return lowercase (Plan A set enum string values, so they do).

Run the test → PASS.

- [ ] **Step 4: Add a `harness-cli notify-watch` subcommand?** Only if `cli/watch.go` is itself exposed as a `watch` subcommand in `cmd/harness-cli/main.go` and you want parity — OPTIONAL, low priority. If you add it, mirror the `watch` case exactly. Otherwise skip (TUI/WebUI consume the method directly).

- [ ] **Step 5: Build + commit.** `go build ./...`; `go test ./cli/ -run 'TestDrainNotifyEvents|TestNotify' -v` pass.
```bash
git add cli/notify_watch.go cli/notify_watch_test.go
git commit -m "feat(cli): WatchNotifications helper (subscribe + decode NotifyEvent stream)"
```

---

### Task 4: E2E — egress + live replay

**Files:** Create `integration/notify_e2e_test.go`.

- [ ] **Step 1: Read context.** Read `integration/e2e_test.go` — mirror its server bring-up (`server.New(server.Config{Addr, DataDir})`, `go s.Run(ctx)`, the `objproto.ParseConnectionID("ws:"+addr+"-*", AllowRandomID|ResolveAddr)` cid, `cli.Dial`). Use a fresh port (e.g. `127.0.0.1:18551`) distinct from other tests.

- [ ] **Step 2: Write the E2E test(s).** Two sub-tests (guard with `if testing.Short() { t.Skip(...) }` like the sibling):

  **(a) Egress:** start `server.New(Config{Addr, DataDir, NotifyHook: <temp script>})` where the script writes its stdin to a temp file. Dial a client, `cli.Notify(ctx, peerCID, "info", "t", "hello-egress")`. Poll the temp file (up to ~3s) and assert it contains `"text":"hello-egress"` and `"level":"info"`.

  **(b) Live replay + live:** start `server.New(Config{Addr, DataDir})` (no hook). Dial client A, send `cli.Notify(... "backlog-1")`. THEN dial client B and `B.WatchNotifications(ctx, &buf)` (run in a goroutine; give it ~500ms). Assert `buf` receives the `backlog-1` event (replayed from the ring on subscribe). Then send `cli.Notify(... "live-2")` from A and assert `buf` also receives `live-2` (live fan-out). Use a mutex around `buf` reads or use a synchronized writer.

  (Build the temp hook script with `os.WriteFile(script, []byte("#!/bin/sh\ncat > "+outFile+"\n"), 0o755)` as in `server/notify_test.go`.)

- [ ] **Step 3: Run + commit.** `go test ./integration/ -run TestNotify -v` (NOT `-short`) passes; `go test ./...` green (the E2E may be skipped under `-short`, that's fine). Confirm no leaked goroutines hang the test (cancel ctx + brief wait, mirroring the sibling's teardown).
```bash
git add integration/notify_e2e_test.go
git commit -m "test(integration): notify E2E — egress hook + live ring replay + live fan-out"
```

---

### Task 5: TUI — display + send

**Files:** `tui/events.go`, `tui/notify.go` (new), `tui/app.go`.

- [ ] **Step 1: Read context (MANDATORY sibling grep).** Read `tui/events.go` (`subscribeAndStream`, `SubscribeTaskLog` — the EXACT helper signature and decode-advance loop), `tui/app.go` (the `App` struct, `Update` routing + focus cycle, an existing `Do*` send command threading `a.client`, the `LogsModel` as a pane template, and where `followTask` starts `SubscribeTaskLog`). Mirror these precisely — do NOT invent a new streaming pattern.

- [ ] **Step 2: Events.** In `tui/events.go` add:
```go
type NotifyEventMsg struct{ Event protocol.NotifyEvent }

func SubscribeNotifications(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.Notifications(), program, func(payload []byte) tea.Msg {
		var ev protocol.NotifyEvent
		if _, err := ev.Decode(payload); err != nil {
			return nil
		}
		return NotifyEventMsg{Event: ev}
	})
}
```
IMPORTANT: match the REAL `subscribeAndStream` signature + how `fn` advances the buffer (read it — the Explore-provided sketch may differ from the actual decode-advance contract). If `subscribeAndStream`'s `fn` is expected to consume exactly one record per call and signal remaining bytes differently, adapt `SubscribeNotifications` to that real contract.

- [ ] **Step 3: Pane model.** Create `tui/notify.go` with a `NotifyModel` mirroring `LogsModel`'s structure (a bounded slice of recent rendered lines + `Update`/`View`). Render each event as e.g. `HH:MM:SS [level] title — text  (origin/host)`. Keep the last ~200.

- [ ] **Step 4: Wire into App.** In `tui/app.go`: add a `notify NotifyModel` field + a `focusNotify` focus; route it in `Update`'s focus switch and `cycleFocus`; handle `NotifyEventMsg` (append to `a.notify`); start `SubscribeNotifications` once the client+program are ready (mirror how `followTask`/initial subscriptions are kicked — likely in the init/connected path). Add a send path: a `DoNotify(c *cli.Client, level, title, text string) tea.Cmd` (mirror `DoCancel`, calling `c.Notify(...)` — the long-lived method, NOT a fresh dial) and a cmdline command `notify <text>` (and/or `notify:<level> <text>`) parsed in the cmdline handler next to the existing commands. Render the pane in `View`.

- [ ] **Step 5: Build + commit.** `make check` (TUI is part of `go build ./...`); `go vet ./tui/`. Manual smoke optional. 
```bash
git add tui/events.go tui/notify.go tui/app.go
git commit -m "feat(tui): notifications pane (live subscribe) + notify send command"
```

---

### Task 6: WebUI — display + send

**Files:** `cmd/harness-webui-wasm/main.go`, `webui/static/main.js`, `webui/index.html`.

- [ ] **Step 1: Read context (MANDATORY sibling grep).** Read `cmd/harness-webui-wasm/main.go` — the `harness` export map, `harnessCancel` (a send action → `currentClient()` + a `cli` call), `harnessWatch` + `watchPipe` (the line-pumping `io.Writer` that calls `js.Global().Call("harness_onTaskEvent", …)`). Read `webui/static/main.js` watch registration + `registerOnConnected`, and `webui/index.html` an existing button/section.

- [ ] **Step 2: wasm exports.** In `cmd/harness-webui-wasm/main.go`:
  - Add `"sendNotification": js.FuncOf(harnessSendNotification)` and `"watchNotifications": js.FuncOf(harnessWatchNotifications)` to the export map.
  - `harnessSendNotification(this, args)`: read `args[0]` object fields `level/title/text` (mirror how `harnessSubmit` reads its args object), then in a goroutine `currentClient()` → `c.Notify(rootCtx, level, title, text)`, resolve/reject a Promise (mirror `harnessCancel`).
  - `harnessWatchNotifications(this, args)`: mirror `harnessWatch` but call `c.WatchNotifications(rootCtx, pipe)` with a `notifyPipe` `io.Writer` that splits on `\n` and calls `js.Global().Call("harness_onNotifyEvent", line)` (each line is already a JSON object from `WatchNotifications`). (Reuse `watchPipe`'s line-splitting; just change the callback name.)

- [ ] **Step 3: rebuild wasm.** Run `make webui-build` (regenerates the wasm module + wasm_exec.js). Confirm `make wasm-check` (`GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/`) passes.

- [ ] **Step 4: JS + HTML.** In `webui/index.html` add a notifications section: a send form (`level` select, `title`, `text` inputs, send button) and a `<pre id="notify-feed">` or list. In `webui/static/main.js`: register `window.harness_onNotifyEvent = (jsonStr) => { … parse … prepend to #notify-feed … }`; in a `registerOnConnected(...)` block call `window.harness.watchNotifications().catch(...)` (mirror the existing watch registration); wire the send button to `await window.harness.sendNotification({level,title,text})`.

- [ ] **Step 5: Build + commit.** `make check && make wasm-check`. Commit the wasm artifact alongside source if the repo commits built wasm (check whether `webui/static/*.wasm` is tracked — mirror what Plan A / the repo convention does; if built wasm is gitignored, commit only source).
```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js webui/index.html
# + the built wasm/wasm_exec.js ONLY if the repo tracks them
git commit -m "feat(webui): notifications feed (watch) + send form"
```

---

## Self-review

**Spec coverage (§8a live leg consumers):**
- Replay-on-subscribe over the `notifications` trsf stream (segmented, not MTU-bound), in-lock so backlog precedes live → Task 1 (hook) + Task 2 (server wiring). ✅ Avoids the recv/accept-queue wedge path (send-only). 
- cli subscribe + decode → Task 3. ✅
- TUI live view + send (reusing `(*Client).Notify`) → Task 5. ✅
- WebUI live view + send (wasm) → Task 6. ✅
- E2E both legs → Task 4 (egress hook + live replay + live fan-out). ✅ (the §12 E2E items deferred from Plan A land here.)

**Ordering rationale:** Tasks 1–4 are headlessly testable (backend + E2E) and land the live leg + the user-requested E2E first; 5–6 are UI. A stop after Task 4 still yields a complete, tested live leg.

**Risk flags for implementers:**
- `AppendData` signature: Task 2's interface MUST match `trsf.BidirectionalStream.AppendData` exactly (grep before writing).
- `subscribeAndStream` real contract: Task 5 must read the actual helper; the sketch may differ on buffer advancement.
- pubsub lock: the replay runs inside `ps.m`; it only writes to the new subscriber's send stream — never read/block (prior wedge incident).
- Long-lived client reuse: TUI/WebUI send via `(*Client).Notify`, never a fresh dial.
