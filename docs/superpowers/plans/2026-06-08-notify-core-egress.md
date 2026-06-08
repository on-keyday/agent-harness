# notify (core + egress + send) Implementation Plan — Plan A

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Project preconditions for EVERY subagent (see `.claude/skills/implementation-pitfalls/SKILL.md`):**
> - Work in the repo this plan is executed against; verify branch with `git rev-parse --abbrev-ref HEAD` before writing. Inside a harness worktree, absolute paths under the parent repo route to the parent checkout — anchor to the checkout you intend.
> - Read `.claude/skills/implementation-pitfalls/SKILL.md` in full first.
> - Build hygiene: compile-check with `make check` / `go build ./...` (no artifacts). NEVER bare `go build ./cmd/<x>/` (drops a binary in the tree). `go test ./...` cleans up after itself.
> - This plan implements the spec `docs/superpowers/specs/2026-06-08-notify-egress-hook-design.md`. **Problem statement** to satisfy (§1): a running task — or the operator from another shell — must be able to push a short text notification that reaches the operator (via an operator-supplied external command) when no WebUI/TUI client is attached, with secrets kept out of the repo.

**Goal:** `harness-cli notify "text"` sends a `TaskControlKind.notify` message to the server, which (a) records it to an in-memory ring + publishes it to the `notifications` pubsub topic, and (b) if `--notify-hook` is configured, spawns that external command (stdin JSON + env) to deliver the notification onward (phone).

**Architecture:** Reuse the existing `TaskControlKind` request/response RPC channel (`RoundTripTaskControl`) and the existing pubsub publish path. One new wire variant (`notify`) plus a server-side exec hook (greenfield — the server has not spawned external processes before) and a small in-memory ring. The live-leg *consumers* (TUI/WebUI display, ring replay-on-subscribe) are deferred to Plan B; this plan produces the full server + egress + send path, independently usable.

**Tech Stack:** Go; `.bgn` schema regenerated via `make protoregen`; standard `testing`.

**Scope note:** This is Plan A of two. Plan B (`notify — live view`) adds the cli watch helper, ring replay-on-subscribe, and TUI/WebUI display+send. The complete notify schema (including `NotifyEvent`, consumed by Plan B) is authored here in Task 1 — schema is never split across tasks/plans.

---

## File structure

| File | Responsibility | Task |
|------|----------------|------|
| `runner/protocol/message.bgn` (+ regenerated `message.go`) | wire schema: `notify` kind, `NotifyLevel/Origin/Status`, `WorkerInfo`, `NotifyRequest/Response/Event` | 1 |
| `cli/notify.go` (new) | build `NotifyRequest` from env, MTU truncate guard, `(*Client).Notify` + free `Notify` | 2 |
| `cli/notify_test.go` (new) | origin-from-env + truncate guard tests | 2 |
| `cmd/harness-cli/main.go` | `notify` subcommand + flags + usage line | 3 |
| `cmd/harness-server/main.go` | `--notify-hook` flag + `HARNESS_NOTIFY_HOOK` fallback → `Config.NotifyHook` | 4 |
| `server/server.go` (`Config`, `Server`, `New`) | `Config.NotifyHook`; `Server.notifyRing`; wire `TaskHandler.NotifyHook` + `OnNotify` | 4, 6 |
| `server/notify_hook.go` (new) | `notifyHookPayload`, `runNotifyHook` (exec, stdin JSON, env, timeout, status) | 5 |
| `server/notify_ring.go` (new) | `notifyRing` (in-memory last-N `NotifyEvent`) | 5 |
| `server/notify_test.go` (new) | hook exec + ring + handler tests | 5, 6 |
| `server/task_handler.go` (`TaskHandler`, `Handle`) | `NotifyHook`/`OnNotify` fields; `handleNotify`; switch case | 6 |
| `topics/topics.go` | `Notifications()` topic constant | 6 |
| `.claude/skills/harness-cli/SKILL.md` | `notify` one-line brevity norm | 7 |

---

### Task 1: Wire schema (complete notify protocol)

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)

- [ ] **Step 1: Add the `notify` kind and all notify formats to the schema**

In `runner/protocol/message.bgn`, append `notify` to the existing `TaskControlKind` enum (after `open_port_forward`):

```
    open_port_forward
    notify
```

Add the two match arms. In `format TaskControlRequest:` (the `match kind:` block, after the `open_port_forward` arm, before the `.. => error(...)` line):

```
        TaskControlKind.notify => notify :NotifyRequest
```

In `format TaskControlResponse:` (the `match kind:` block, after the `open_port_forward` arm):

```
        TaskControlKind.notify => notify :NotifyResponse
```

Add the new enums and formats (place them immediately after `format TaskControlResponse:` … its match block, near the existing `StatusEventKind`/`TaskStatusEvent` definitions):

```
enum NotifyLevel:
    :u8
    info
    warn
    error

enum NotifyOrigin:
    :u8
    worker
    external

enum NotifyStatus:
    :u8
    accepted
    no_hook
    spawn_failed

format WorkerInfo:
    task_id_len   :u16
    task_id       :[task_id_len]u8
    runner_id_len :u16
    runner_id     :[runner_id_len]u8
    repo_len      :u16
    repo          :[repo_len]u8
    hostname_len  :u16
    hostname      :[hostname_len]u8

format NotifyRequest:
    level  :NotifyLevel
    origin :NotifyOrigin
    if origin == NotifyOrigin.worker:
        worker :WorkerInfo
    title_len :u16
    title     :[title_len]u8
    text_len  :u16
    text      :[text_len]u8

format NotifyResponse:
    status :NotifyStatus

format NotifyEvent:
    ts          :u64
    client_kind :ClientKind
    level  :NotifyLevel
    origin :NotifyOrigin
    if origin == NotifyOrigin.worker:
        worker :WorkerInfo
    title_len :u16
    title     :[title_len]u8
    text_len  :u16
    text      :[text_len]u8
```

- [ ] **Step 2: Regenerate Go from the schema**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: regenerates `runner/protocol/message.go` with no error (first run downloads brgen-kit, ~10s).

- [ ] **Step 3: Confirm the generated accessors exist**

Run: `grep -nE 'func .*TaskControlRequest\) (Notify|SetNotify)\(|func .*NotifyRequest\) (Worker|SetWorker)\(|NotifyStatus_Accepted|NotifyStatus_NoHook|NotifyStatus_SpawnFailed|NotifyLevel_Info|NotifyOrigin_Worker|func .*NotifyEvent\) MustAppend' runner/protocol/message.go`
Expected: matches for `Notify()`, `SetNotify(...)`, `Worker()`, `SetWorker(...)`, the three `NotifyStatus_*` constants, `NotifyLevel_Info`, `NotifyOrigin_Worker`, and `NotifyEvent.MustAppend`. (Generator convention: `task_id_len`→`TaskIdLen`, `no_hook`→`NotifyStatus_NoHook`.)

- [ ] **Step 4: Compile-check**

Run: `go build ./...`
Expected: builds clean (no consumers yet — this only confirms the generated code compiles).

- [ ] **Step 5: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "feat(proto): notify wire schema (TaskControlKind.notify + NotifyRequest/Response/Event)"
```

---

### Task 2: cli helper — `(*Client).Notify` + free `Notify` + MTU guard

**Files:**
- Create: `cli/notify.go`
- Test: `cli/notify_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cli/notify_test.go`:

```go
package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestNewNotifyRequestFromEnv_Worker(t *testing.T) {
	t.Setenv("HARNESS_TASK_ID", "0f0d4dd6")
	t.Setenv("HARNESS_RUNNER_ID", "ws:10.0.0.1:1-2")
	t.Setenv("HARNESS_REPO_PATH", "/repo")
	t.Setenv("HARNESS_HOSTNAME", "host1")

	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Warn, "title", "body")
	if nr.Origin != protocol.NotifyOrigin_Worker {
		t.Fatalf("origin = %v, want worker", nr.Origin)
	}
	w := nr.Worker()
	if w == nil {
		t.Fatal("Worker() is nil for worker origin")
	}
	if string(w.TaskId) != "0f0d4dd6" || string(w.Hostname) != "host1" {
		t.Fatalf("worker fields wrong: task_id=%q hostname=%q", w.TaskId, w.Hostname)
	}
	if string(nr.Text) != "body" || string(nr.Title) != "title" {
		t.Fatalf("title/text wrong: %q / %q", nr.Title, nr.Text)
	}
}

func TestNewNotifyRequestFromEnv_External(t *testing.T) {
	os.Unsetenv("HARNESS_TASK_ID")
	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", "hi")
	if nr.Origin != protocol.NotifyOrigin_External {
		t.Fatalf("origin = %v, want external", nr.Origin)
	}
	if nr.Worker() != nil {
		t.Fatal("Worker() must be nil for external origin")
	}
}

func TestMtuGuardNotify_Truncates(t *testing.T) {
	os.Unsetenv("HARNESS_TASK_ID")
	long := strings.Repeat("あ", 2000) // ~6 KB UTF-8, far over budget
	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", long)
	mtuGuardNotify(nr)

	if encodedNotifyWireLen(nr) > notifyWireBudget {
		t.Fatalf("after guard encoded len %d > budget %d", encodedNotifyWireLen(nr), notifyWireBudget)
	}
	if !strings.HasSuffix(string(nr.Text), "…") {
		t.Fatalf("truncated text must end with ellipsis, got tail %q", tail(string(nr.Text)))
	}
	// rune boundary preserved: text is valid UTF-8
	if !isValidUTF8(nr.Text) {
		t.Fatal("truncation split a UTF-8 rune")
	}
}

func TestMtuGuardNotify_ShortNoop(t *testing.T) {
	os.Unsetenv("HARNESS_TASK_ID")
	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", "short")
	mtuGuardNotify(nr)
	if string(nr.Text) != "short" {
		t.Fatalf("short text must be untouched, got %q", nr.Text)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cli/ -run TestNewNotifyRequestFromEnv -v`
Expected: FAIL — `newNotifyRequestFromEnv` / `mtuGuardNotify` / helpers undefined.

- [ ] **Step 3: Implement `cli/notify.go`**

Create `cli/notify.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"unicode/utf8"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyWireBudget is a conservative cap on the encoded TaskControlRequest
// carrying a NotifyRequest. RoundTripTaskControl sends it as a single objproto
// message, which on UDP transport is path-MTU-bound (trsf.DefaultInitialMTU =
// 1200). An oversize message does not arrive on UDP, so the bound is enforced
// here, client-side, before send. 1000 leaves headroom for objproto/trsf framing.
const notifyWireBudget = 1000

// parseNotifyLevel maps the CLI --level string to a NotifyLevel; empty → info.
func parseNotifyLevel(s string) (protocol.NotifyLevel, error) {
	switch s {
	case "", "info":
		return protocol.NotifyLevel_Info, nil
	case "warn":
		return protocol.NotifyLevel_Warn, nil
	case "error":
		return protocol.NotifyLevel_Error, nil
	default:
		return 0, fmt.Errorf("invalid --level %q (want info|warn|error)", s)
	}
}

// newNotifyRequestFromEnv builds a NotifyRequest, deriving origin from the
// HARNESS_* env set by the runner inside a worker. HARNESS_TASK_ID present →
// origin=worker with the WorkerInfo block; absent → origin=external.
func newNotifyRequestFromEnv(level protocol.NotifyLevel, title, text string) *protocol.NotifyRequest {
	nr := &protocol.NotifyRequest{Level: level}
	if taskID := os.Getenv("HARNESS_TASK_ID"); taskID != "" {
		nr.Origin = protocol.NotifyOrigin_Worker
		runnerID := os.Getenv("HARNESS_RUNNER_ID")
		repo := os.Getenv("HARNESS_REPO_PATH")
		host := os.Getenv("HARNESS_HOSTNAME")
		nr.SetWorker(protocol.WorkerInfo{
			TaskIdLen:   uint16(len(taskID)),
			TaskId:      []byte(taskID),
			RunnerIdLen: uint16(len(runnerID)),
			RunnerId:    []byte(runnerID),
			RepoLen:     uint16(len(repo)),
			Repo:        []byte(repo),
			HostnameLen: uint16(len(host)),
			Hostname:    []byte(host),
		})
	} else {
		nr.Origin = protocol.NotifyOrigin_External
	}
	nr.TitleLen = uint16(len(title))
	nr.Title = []byte(title)
	nr.TextLen = uint16(len(text))
	nr.Text = []byte(text)
	return nr
}

// encodedNotifyWireLen measures the wire size of the TaskControlRequest that
// would carry nr (AppKind byte + kind + request_id + NotifyRequest body).
func encodedNotifyWireLen(nr *protocol.NotifyRequest) int {
	tcr := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify}
	tcr.SetNotify(*nr)
	return len(tcr.MustAppend([]byte{byte(appwire.AppKind_TaskControl)}))
}

// mtuGuardNotify truncates nr.Text (rune-safe, with a trailing ellipsis) so the
// encoded message fits notifyWireBudget, and warns on stderr. No-op if it fits.
func mtuGuardNotify(nr *protocol.NotifyRequest) {
	if encodedNotifyWireLen(nr) <= notifyWireBudget {
		return
	}
	original := string(nr.Text)
	// measure overhead with empty text
	nr.Text = nil
	nr.TextLen = 0
	overhead := encodedNotifyWireLen(nr)
	const ell = "…"
	maxText := notifyWireBudget - overhead - len(ell)
	if maxText < 0 {
		maxText = 0
	}
	trimmed := truncateRunes(original, maxText) + ell
	nr.Text = []byte(trimmed)
	nr.TextLen = uint16(len(nr.Text))
	fmt.Fprintf(os.Stderr, "notify: text truncated %d→%d bytes to fit transport MTU\n", len(original), len(nr.Text))
}

// truncateRunes returns the longest prefix of s that is <= maxBytes and ends on
// a UTF-8 rune boundary.
func truncateRunes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s)
	n := maxBytes
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	return string(b[:n])
}

// isValidUTF8 / tail are test helpers kept here so the test file needs no extra imports.
func isValidUTF8(b []byte) bool { return utf8.Valid(b) }
func tail(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[len(s)-12:]
}

// Notify sends a notification over an existing *Client. Long-lived consumers
// (TUI/WebUI) call this on their persistent client. level is "info|warn|error".
func (c *Client) Notify(ctx context.Context, level, title, text string) error {
	lvl, err := parseNotifyLevel(level)
	if err != nil {
		return err
	}
	nr := newNotifyRequestFromEnv(lvl, title, text)
	mtuGuardNotify(nr)

	tcr := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify}
	tcr.SetNotify(*nr)
	resp, err := c.RoundTripTaskControl(ctx, tcr)
	if err != nil {
		return err
	}
	if resp.Kind != protocol.TaskControlKind_Notify {
		return fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	out := resp.Notify()
	if out == nil {
		return fmt.Errorf("nil notify response")
	}
	switch out.Status {
	case protocol.NotifyStatus_NoHook:
		fmt.Fprintln(os.Stderr, "notify: server has no --notify-hook configured (recorded live-only, no external delivery)")
	case protocol.NotifyStatus_SpawnFailed:
		return fmt.Errorf("notify: server failed to spawn the configured hook")
	}
	return nil
}

// Notify (package-level) opens a fresh Client per call — for short-lived
// harness-cli. Long-lived consumers should hold a *Client and call the method.
func Notify(ctx context.Context, peerCID objproto.ConnectionID, level, title, text string) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Notify(ctx, level, title, text)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cli/ -run 'TestNewNotifyRequestFromEnv|TestMtuGuardNotify' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Compile-check + commit**

Run: `go build ./...`
Expected: clean.

```bash
git add cli/notify.go cli/notify_test.go
git commit -m "feat(cli): notify helper with env-origin + client-side MTU truncate guard"
```

---

### Task 3: `harness-cli notify` subcommand

**Files:**
- Modify: `cmd/harness-cli/main.go` (subcommand switch + `usage()`)

- [ ] **Step 1: Add the `notify` case to the subcommand switch**

In `cmd/harness-cli/main.go`, in the main `switch` on the subcommand (next to `case "cancel":`), add:

```go
	case "notify":
		fs := flag.NewFlagSet("notify", flag.ExitOnError)
		title := fs.String("title", "", "short heading for the notification")
		level := fs.String("level", "info", "severity: info|warn|error")
		_ = fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "notify: missing text")
			os.Exit(2)
		}
		text := strings.Join(rest, " ")
		if err := cli.Notify(ctx, parseCID(), *level, *title, text); err != nil {
			die(err)
		}
```

(If `strings` is not already imported in this file, add it to the import block.)

- [ ] **Step 2: Add a usage line**

In the `usage()` function's printed list of subcommands, add a line near `cancel`:

```go
	fmt.Fprintln(os.Stderr, "  notify [--title T] [--level info|warn|error] <text>   send a notification (one short line; detail goes in the task log)")
```

- [ ] **Step 3: Compile-check**

Run: `go build ./cmd/harness-cli/... && go vet ./cmd/harness-cli/`
Expected: clean. (Per build hygiene, do NOT run bare `go build ./cmd/harness-cli/` — the `&&`-chained form above compiles without emitting a binary because no `-o` to cwd; if unsure use `go build -o /dev/null ./cmd/harness-cli`.)

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "feat(cli): harness-cli notify subcommand"
```

---

### Task 4: Server config — `--notify-hook`

**Files:**
- Modify: `cmd/harness-server/main.go` (flag + env fallback + Config)
- Modify: `server/server.go` (`Config.NotifyHook`)

- [ ] **Step 1: Add the Config field**

In `server/server.go`, in the `Config` struct, add (after `DetachIdleTimeout`):

```go
	// NotifyHook, when non-empty, is an executable invoked once per notify
	// request: stdin receives a JSON payload, env carries HARNESS_NOTIFY_*.
	// Empty disables the egress leg (notify still records to the ring + topic).
	// Invoked directly (no shell) — text is on stdin, never an argument.
	NotifyHook string
```

- [ ] **Step 2: Add the flag + env fallback + wire to Config**

In `cmd/harness-server/main.go`, in the flag block (near `listen`):

```go
	notifyHook = flag.String("notify-hook", "", "external command invoked on each notify request (stdin: JSON; env: HARNESS_NOTIFY_*); empty disables egress, fallback env HARNESS_NOTIFY_HOOK")
```

After flags are parsed (near the PSK resolution), resolve the env fallback:

```go
	nh := strings.TrimSpace(*notifyHook)
	if nh == "" {
		nh = strings.TrimSpace(os.Getenv("HARNESS_NOTIFY_HOOK"))
	}
```

In the `server.New(server.Config{...})` literal, add:

```go
		NotifyHook: nh,
```

- [ ] **Step 3: Compile-check + commit**

Run: `go build ./...`
Expected: clean.

```bash
git add cmd/harness-server/main.go server/server.go
git commit -m "feat(server): --notify-hook config (flag + HARNESS_NOTIFY_HOOK fallback)"
```

---

### Task 5: Server exec hook + in-memory ring

**Files:**
- Create: `server/notify_hook.go`
- Create: `server/notify_ring.go`
- Test: `server/notify_test.go`

- [ ] **Step 1: Write the failing tests**

Create `server/notify_test.go`:

```go
package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRunNotifyHook_NoHook(t *testing.T) {
	if got := runNotifyHook("", notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_NoHook {
		t.Fatalf("status = %v, want no_hook", got)
	}
}

func TestRunNotifyHook_SpawnFailed(t *testing.T) {
	if got := runNotifyHook("/nonexistent/notify-hook-xyz", notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_SpawnFailed {
		t.Fatalf("status = %v, want spawn_failed", got)
	}
}

func TestRunNotifyHook_Accepted_DeliversPayload(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "hook.sh")
	// write stdin + the level env to outFile, then exit 0
	body := "#!/bin/sh\ncat > " + outFile + "\necho \"LEVEL=$HARNESS_NOTIFY_LEVEL\" >> " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	status := runNotifyHook(script, notifyHookPayload{Level: "warn", Text: "hello", Origin: "external"})
	if status != protocol.NotifyStatus_Accepted {
		t.Fatalf("status = %v, want accepted", status)
	}
	// runNotifyHook is fire-and-forget; wait briefly for the reap goroutine.
	var data []byte
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			data = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := string(data)
	if !strings.Contains(s, `"text":"hello"`) || !strings.Contains(s, "LEVEL=warn") {
		t.Fatalf("hook did not receive payload/env, got: %q", s)
	}
}

func TestNotifyRing_AppendEvicts(t *testing.T) {
	r := newNotifyRing(3)
	for i := 0; i < 5; i++ {
		r.append(protocol.NotifyEvent{Ts: uint64(i)})
	}
	snap := r.snapshot()
	if len(snap) != 3 {
		t.Fatalf("ring len = %d, want 3", len(snap))
	}
	if snap[0].Ts != 2 || snap[2].Ts != 4 {
		t.Fatalf("ring kept wrong entries: first=%d last=%d", snap[0].Ts, snap[2].Ts)
	}
}
```

(Add `"time"` to the import block — used by the poll loop.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./server/ -run 'TestRunNotifyHook|TestNotifyRing' -v`
Expected: FAIL — `runNotifyHook` / `notifyHookPayload` / `newNotifyRing` undefined.

- [ ] **Step 3: Implement the ring**

Create `server/notify_ring.go`:

```go
package server

import (
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyRing is a fixed-capacity in-memory ring of recent NotifyEvents.
// Lost on restart — no disk persistence (spec §8a). Consumed by Plan B's
// replay-on-subscribe.
type notifyRing struct {
	mu  sync.Mutex
	buf []protocol.NotifyEvent
	cap int
}

func newNotifyRing(capacity int) *notifyRing {
	if capacity < 1 {
		capacity = 1
	}
	return &notifyRing{cap: capacity}
}

func (r *notifyRing) append(ev protocol.NotifyEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, ev)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

// snapshot returns a copy of the current ring contents, oldest first.
func (r *notifyRing) snapshot() []protocol.NotifyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.NotifyEvent, len(r.buf))
	copy(out, r.buf)
	return out
}
```

- [ ] **Step 4: Implement the hook**

Create `server/notify_hook.go`:

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyHookTimeout bounds a hook process; a slow/hung sink must not pile up.
const notifyHookTimeout = 10 * time.Second

// notifyHookPayload is the JSON written to the hook's stdin. Worker fields are
// empty for origin=external. conn_id + ts are server-injected.
type notifyHookPayload struct {
	Level    string `json:"level"`
	Origin   string `json:"origin"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	TaskID   string `json:"task_id,omitempty"`
	RunnerID string `json:"runner_id,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	ConnID   string `json:"conn_id"`
	Ts       int64  `json:"ts"`
}

// runNotifyHook launches hookCmd (no shell), passing payload as stdin JSON and
// HARNESS_NOTIFY_* env. It does NOT wait for completion: Start success →
// accepted (launched, not delivered); the process is reaped + timeout-killed in
// a background goroutine. Empty hookCmd → no_hook; Start failure → spawn_failed.
func runNotifyHook(hookCmd string, payload notifyHookPayload) protocol.NotifyStatus {
	if hookCmd == "" {
		return protocol.NotifyStatus_NoHook
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("notify hook: marshal payload", "err", err)
		return protocol.NotifyStatus_SpawnFailed
	}
	// Background context so the hook outlives the request handler.
	cctx, cancel := context.WithTimeout(context.Background(), notifyHookTimeout)
	cmd := exec.CommandContext(cctx, hookCmd)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(),
		"HARNESS_NOTIFY_LEVEL="+payload.Level,
		"HARNESS_NOTIFY_ORIGIN="+payload.Origin,
		"HARNESS_NOTIFY_TITLE="+payload.Title,
	)
	if err := cmd.Start(); err != nil {
		cancel()
		slog.Error("notify hook: spawn failed", "cmd", hookCmd, "err", err)
		return protocol.NotifyStatus_SpawnFailed
	}
	go func() {
		defer cancel()
		if err := cmd.Wait(); err != nil {
			slog.Warn("notify hook: nonzero/timeout", "cmd", hookCmd, "err", err)
		}
	}()
	return protocol.NotifyStatus_Accepted
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./server/ -run 'TestRunNotifyHook|TestNotifyRing' -v`
Expected: PASS (4 tests).

- [ ] **Step 6: Commit**

```bash
git add server/notify_hook.go server/notify_ring.go server/notify_test.go
git commit -m "feat(server): notify exec hook (stdin JSON + env, fire-and-forget) + in-memory ring"
```

---

### Task 6: Handler — `handleNotify` + server wiring

**Files:**
- Modify: `server/task_handler.go` (`TaskHandler` fields, switch case, `handleNotify`)
- Modify: `server/server.go` (`Server.notifyRing`, wiring in `New`)
- Modify: `topics/topics.go` (`Notifications()`)
- Test: `server/notify_test.go` (append handler test)

- [ ] **Step 1: Add the topic constant**

In `topics/topics.go`, add:

```go
func Notifications() string { return "notifications" }
```

- [ ] **Step 2: Add TaskHandler fields**

In `server/task_handler.go`, in the `TaskHandler` struct (near `OnChange`), add:

```go
	// NotifyHook is the configured external command for the egress leg of
	// notify (empty = egress disabled). See server/notify_hook.go.
	NotifyHook string

	// OnNotify runs the live leg for a notify (ring append + topic publish).
	// nil-safe: tests may leave it nil to exercise egress in isolation.
	OnNotify func(ev protocol.NotifyEvent)
```

- [ ] **Step 3: Write the failing handler test**

Append to `server/notify_test.go`:

```go
func TestHandleNotify_NoHook_RunsLiveLeg(t *testing.T) {
	var captured *protocol.NotifyEvent
	h := &TaskHandler{
		OnNotify: func(ev protocol.NotifyEvent) { captured = &ev },
		// NotifyHook empty → egress disabled
	}

	nr := protocol.NotifyRequest{Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_External}
	nr.TextLen = uint16(len("hi"))
	nr.Text = []byte("hi")
	req := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify, RequestId: 7}
	req.SetNotify(nr)

	conn := &captureConn{}
	h.handleNotify(conn, &req)

	if captured == nil {
		t.Fatal("OnNotify (live leg) was not called")
	}
	if string(captured.Text) != "hi" || captured.Level != protocol.NotifyLevel_Warn {
		t.Fatalf("event wrong: text=%q level=%v", captured.Text, captured.Level)
	}
	// response decoded back must be notify/no_hook
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(conn.last[1:]); err != nil { // strip AppKind byte
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_Notify || resp.RequestId != 7 {
		t.Fatalf("response kind/id wrong: %v/%d", resp.Kind, resp.RequestId)
	}
	if out := resp.Notify(); out == nil || out.Status != protocol.NotifyStatus_NoHook {
		t.Fatalf("status = %v, want no_hook", out)
	}
}
```

Add a minimal `ConnHandle` stub at the top of `server/notify_test.go` (after imports):

```go
// captureConn is a minimal ConnHandle that records the last SendMessage.
type captureConn struct{ last []byte }

func (c *captureConn) ConnectionID() objproto.ConnectionID { return objproto.ConnectionID{} }
func (c *captureConn) SendMessage(b []byte) (int, uint64, error) {
	c.last = append([]byte(nil), b...)
	return len(b), 0, nil
}
func (c *captureConn) CreateSendStream() trsf.SendStream                       { return nil }
func (c *captureConn) CreateBidirectionalStream() trsf.BidirectionalStream     { return nil }
func (c *captureConn) GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream    { return nil }
func (c *captureConn) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return nil
}
```

(Add imports `"github.com/on-keyday/objtrsf/objproto"` and `"github.com/on-keyday/objtrsf/trsf"` to the test file. Confirm the exact `trsf` import path by grepping an existing server test, e.g. `grep -n 'objtrsf/trsf"' server/*.go | head -1`, and match it.)

- [ ] **Step 4: Run the handler test to verify it fails**

Run: `go test ./server/ -run TestHandleNotify -v`
Expected: FAIL — `handleNotify` undefined.

- [ ] **Step 5: Implement `handleNotify` + the switch case**

In `server/task_handler.go`, in the `switch req.Kind` inside `Handle`, add:

```go
	case protocol.TaskControlKind_Notify:
		h.handleNotify(conn, &req)
```

Add the method (anywhere in `server/task_handler.go`):

```go
// handleNotify runs both legs of a notify request: the live leg (OnNotify →
// ring + topic) and the egress leg (NotifyHook exec), then replies with the
// resulting NotifyStatus. accepted = hook launched; no_hook = egress disabled
// (live leg still ran); spawn_failed = hook failed to start.
func (h *TaskHandler) handleNotify(conn ConnHandle, req *protocol.TaskControlRequest) {
	nr := req.Notify()
	if nr == nil {
		slog.Error("TaskHandler: Notify variant is nil")
		return
	}
	cid := conn.ConnectionID().String()
	h.clientKindsMu.Lock()
	ck := h.clientKinds[cid]
	h.clientKindsMu.Unlock()

	ts := time.Now().UnixNano()
	ev := protocol.NotifyEvent{
		Ts:         uint64(ts),
		ClientKind: ck,
		Level:      nr.Level,
		Origin:     nr.Origin,
		TitleLen:   nr.TitleLen,
		Title:      nr.Title,
		TextLen:    nr.TextLen,
		Text:       nr.Text,
	}
	if nr.Origin == protocol.NotifyOrigin_Worker {
		if w := nr.Worker(); w != nil {
			ev.SetWorker(*w)
		}
	}

	// live leg
	if h.OnNotify != nil {
		h.OnNotify(ev)
	}

	// egress leg
	payload := notifyHookPayload{
		Level:  nr.Level.String(),
		Origin: nr.Origin.String(),
		Title:  string(nr.Title),
		Text:   string(nr.Text),
		ConnID: cid,
		Ts:     ts,
	}
	if w := nr.Worker(); w != nil {
		payload.TaskID = string(w.TaskId)
		payload.RunnerID = string(w.RunnerId)
		payload.Repo = string(w.Repo)
		payload.Hostname = string(w.Hostname)
	}
	status := runNotifyHook(h.NotifyHook, payload)

	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Notify, RequestId: req.RequestId}
	resp.SetNotify(protocol.NotifyResponse{Status: status})
	out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
	conn.SendMessage(out) //nolint:errcheck
}
```

(`nr.Level.String()` / `nr.Origin.String()` are generated enum stringers — confirm with `grep -n 'func (.*NotifyLevel) String' runner/protocol/message.go`. If the generated stringer yields capitalized/qualified text rather than `"info"`/`"worker"`, replace these two with a small local `switch` mapping to the lowercase wire words used in the spec's JSON.)

- [ ] **Step 6: Wire the ring + OnNotify in the server**

In `server/server.go`, add a field to the `Server` struct (near `taskHandler`):

```go
	notifyRing *notifyRing
```

In `New`, where `s.taskHandler` is constructed and the `publishTaskEvent` closure is wired, add (after `s.taskHandler = &TaskHandler{...}`):

```go
	s.notifyRing = newNotifyRing(64)
	s.taskHandler.NotifyHook = cfg.NotifyHook
	s.taskHandler.OnNotify = func(ev protocol.NotifyEvent) {
		s.notifyRing.append(ev)
		s.pubsub.Publish("server", topics.Notifications(), ev.MustAppend(nil))
	}
```

- [ ] **Step 7: Run all notify tests + full build/test**

Run: `go test ./server/ -run 'TestRunNotifyHook|TestNotifyRing|TestHandleNotify' -v`
Expected: PASS.

Run: `make check && go test ./...`
Expected: build clean; full suite green.

- [ ] **Step 8: Commit**

```bash
git add server/task_handler.go server/server.go server/notify_test.go topics/topics.go
git commit -m "feat(server): handleNotify — live leg (ring+topic) + egress leg (hook), notify wired end-to-end"
```

---

### Task 7: Document the brevity norm

**Files:**
- Modify: `.claude/skills/harness-cli/SKILL.md`

- [ ] **Step 1: Add a notify section**

In `.claude/skills/harness-cli/SKILL.md`, add a short subsection documenting the subcommand and the one-line norm:

```markdown
## notify

`harness-cli notify [--title T] [--level info|warn|error] <text>` sends a
notification to the server, which records it (live view) and, if the server was
started with `--notify-hook`, relays it to that external command (→ phone).

**Keep it to one short line.** The server truncates over-long text to fit the
transport; detail belongs in the task log, not the notification. Agents: call it
fire-and-forget and end the turn — it is a one-way ping, not a question.
Origin (task/runner/repo/hostname) is filled automatically from `HARNESS_*` env
when run inside a worker; outside a worker it is marked `external`.
```

- [ ] **Step 2: Commit**

```bash
git add .claude/skills/harness-cli/SKILL.md
git commit -m "docs(harness-cli): document notify subcommand + one-line brevity norm"
```

---

## Self-review

**Spec coverage (Plan A scope):**
- Problem statement (§1: task/operator pushes a short notification reaching the operator via external command when no client attached, secrets out of repo) → Tasks 2/3 (send), 4 (config), 5/6 (egress hook). ✅
- Schema §4 (complete, incl. NotifyEvent) → Task 1 (one task, not split). ✅
- Origin from env, worker/external, caller-asserted §5 → Task 2. ✅
- Hook contract §6 (no shell, stdin JSON, env, timeout, accepted=launched, no_hook/spawn_failed) → Task 5/6. ✅
- MTU truncate guard, client-side, rune-safe, stderr warn §7 → Task 2. ✅
- Fire-and-forget + turn-end ergonomics §8; `(*Client).Notify` reuse form for TUI/WebUI → Task 2 (method is the reuse form, mirroring `(*Client).Cancel`). ✅
- Live leg production (ring + `notifications` topic) §8a → Task 6 (server side). **Consumers (TUI/WebUI display, replay-on-subscribe) are Plan B** — explicitly out of scope here.
- Security/tradeoffs §10 (egress-only, secrets in external script, server-spawn minimized: no shell + stdin + timeout) → Task 5. ✅
- Insertion points §11 covered except the Plan-B rows (TUI/WebUI/replay). ✅

**Deferred to Plan B (not gaps):** `cli` notifications watch helper; ring replay-on-subscribe over the topic stream; TUI notifications pane + send action; WebUI watch/toast + send button (incl. wasm binding). `NotifyEvent` + ring + topic publish are built here so Plan B is pure consumption.

**Placeholder scan:** none — every code step has complete code. Two `grep`-and-confirm steps (Task 1 Step 3, Task 6 Step 5 stringer) verify generated-API naming rather than guess; they have explicit fallbacks.

**Type consistency:** `(*Client).Notify(ctx, level, title, text string)` used identically in Task 2 (def), Task 3 (free `Notify` wrapper). `notifyHookPayload`, `runNotifyHook(hookCmd, payload) NotifyStatus`, `newNotifyRing(n)`, `notifyRing.append/snapshot`, `TaskHandler.OnNotify func(protocol.NotifyEvent)`, `TaskHandler.NotifyHook string`, `topics.Notifications()` — names match across Tasks 5/6. Generated accessors (`SetNotify`/`Notify()`/`Worker()`/`SetWorker`/`NotifyStatus_*`) confirmed against the Cancel pattern + Task 1 Step 3.
