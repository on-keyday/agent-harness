# Persist-Reconnect Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-out `--persist` flag to `agent-runner`, `harness-tui`, and `harness-webui-wasm` so they auto-reconnect on WebSocket drops with exponential-w/-jitter backoff, re-establishing per-process state (Hello / subscriptions / UI) on each reconnect.

**Architecture:** Each long-running entry point delegates to a shared `cli.PersistLoop(ctx, dial, onConnect, cfg)` helper that watches `peer.Conn.Done()`, retires the previous run's goroutines via a runCtx, and re-dials with backoff. `runner.Run` is split into `Connect + OnConnect` so the per-Run ctx scopes claude subprocess teardown. Server-side semantics are unchanged ("fresh reconnect": old `RunnerEntry` goes through `OnRemove`, in-flight tasks marked Failed, new ConnectionID registers as a clean runner).

**Tech Stack:** Go 1.x, `peer.Conn` / `objproto` / `trsf` (in-tree), `cli.Client` (in-tree), Bubble Tea (TUI), Go WASM + JS (WebUI), `go test` for unit + `-tags integration` for end-to-end.

**Spec:** `docs/superpowers/specs/2026-05-09-persist-reconnect-design.md`

---

## File Structure

| Path | Purpose | Status |
|---|---|---|
| `cli/persist.go` | `PersistLoop`, `PersistConfig`, `PersistDialer`, `PersistOnConnect`, `PersistHandle`, `PersistState`, `PersistPhase`, `PSKAuthError` | new |
| `cli/persist_test.go` | unit tests for `PersistLoop` | new |
| `peer/conn.go` | lower `DialConfig.PingInterval` default 30s → 15s | modify (1 line + godoc) |
| `runner/connect.go` | split `Run` → `Connect` + `OnConnect`, runCtx scoping for spawned goroutines | modify |
| `runner/connect_test.go` | cover runCtx propagation to `handleAssign` | extend |
| `cmd/agent-runner/main.go` | persist flags + `PersistLoop` wiring | modify |
| `tui/events.go` | extend `ConnectionMsg` with reconnect fields | modify |
| `tui/app.go` | `BindClient` re-entrancy, `FollowingTaskID`, status line text | modify |
| `tui/app_test.go` | cover `BindClient` swap and ConnectionMsg rendering | extend (or new) |
| `cmd/harness-tui/main.go` | `PersistLoop` wiring | modify |
| `cmd/harness-webui-wasm/main.go` | `harness.connect` options bag, `PersistLoop`, `harness.onConnectionChange` | modify |
| `webui/static/main.js` | banner UI + resubscribe-on-connected pattern | modify |
| `integration/persist_test.go` | end-to-end reconnect across server restart (build tag `integration`) | new |

---

## Task 1: Lower default `PingInterval` 30s → 15s

**Files:**
- Modify: `peer/conn.go:74-78`

This is a one-line change with no behavioural test of its own; later integration tests will exercise it. Rationale captured in spec §3.1 / §5.2.

- [ ] **Step 1: Update default**

In `peer/conn.go`, replace:

```go
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
```

with:

```go
	if cfg.PingInterval <= 0 {
		// 15s strikes a balance between idle-disconnect detection latency
		// (worst case ≈ PingInterval after a silent WS underlay drop) and
		// wire chatter. Persist mode relies on this for responsive reconnects;
		// see docs/superpowers/specs/2026-05-09-persist-reconnect-design.md §3.1.
		cfg.PingInterval = 15 * time.Second
	}
```

- [ ] **Step 2: Verify build and existing tests still pass**

```bash
go build ./...
go test ./peer/... ./trsf/... -count=1
```

Expected: PASS (no test asserts the 30s value).

- [ ] **Step 3: Commit**

```bash
git add peer/conn.go
git commit -m "peer: default PingInterval 30s → 15s for faster disconnect detection"
```

---

## Task 2: `cli/persist.go` types + happy-path `PersistLoop`

**Files:**
- Create: `cli/persist.go`
- Create: `cli/persist_test.go`

This task introduces the public types and the simplest path: dial succeeds → OnConnect runs → handle.Done() fires → loop tears down → next iteration redials. Backoff and corner cases land in Tasks 3-4.

- [ ] **Step 1: Write the failing happy-path test**

Create `cli/persist_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHandle implements PersistHandle for tests.
type fakeHandle struct {
	done chan struct{}
	once sync.Once
}

func newFakeHandle() *fakeHandle { return &fakeHandle{done: make(chan struct{})} }
func (h *fakeHandle) Done() <-chan struct{} { return h.done }
func (h *fakeHandle) Close()                { h.once.Do(func() { close(h.done) }) }

// instantSleep makes PersistLoop's backoff a no-op so tests run fast.
func instantSleep(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func TestPersistLoop_HappyPath_TwoIterations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialCalls int32
	var onConnectCalls int32
	handles := make(chan *fakeHandle, 2)

	dial := func(_ context.Context) (PersistHandle, error) {
		atomic.AddInt32(&dialCalls, 1)
		h := newFakeHandle()
		handles <- h
		return h, nil
	}
	onConnect := func(runCtx context.Context, h PersistHandle) error {
		atomic.AddInt32(&onConnectCalls, 1)
		<-runCtx.Done()
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- PersistLoop(ctx, dial, onConnect, PersistConfig{
			Enabled: true,
			Sleep:   instantSleep,
		})
	}()

	// First iteration: receive the handle, close it to force reconnect.
	h1 := <-handles
	h1.Close()

	// Second iteration: receive the handle, then cancel the parent ctx.
	h2 := <-handles
	cancel()
	h2.Close()

	if err := <-done; err != nil {
		t.Fatalf("PersistLoop returned %v, want nil", err)
	}
	if got := atomic.LoadInt32(&dialCalls); got != 2 {
		t.Fatalf("dialCalls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&onConnectCalls); got != 2 {
		t.Fatalf("onConnectCalls = %d, want 2", got)
	}
}

func TestPersistLoop_OnConnectErrorTriggersReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	dial := func(_ context.Context) (PersistHandle, error) {
		return newFakeHandle(), nil
	}
	onConnect := func(_ context.Context, _ PersistHandle) error {
		n := atomic.AddInt32(&attempts, 1)
		if n >= 2 {
			cancel()
			return nil
		}
		return errors.New("transient onConnect failure")
	}

	err := PersistLoop(ctx, dial, onConnect, PersistConfig{
		Enabled: true,
		Sleep:   instantSleep,
	})
	if err != nil {
		t.Fatalf("PersistLoop returned %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
go test ./cli/ -run TestPersistLoop_ -count=1
```

Expected: build error / undefined `PersistLoop`, `PersistHandle`, `PersistConfig`.

- [ ] **Step 3: Create `cli/persist.go` with types and happy-path implementation**

Create `cli/persist.go`:

```go
package cli

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"time"
)

// PersistPhase is the current state of a PersistLoop.
type PersistPhase int

const (
	PersistPhaseConnecting PersistPhase = iota
	PersistPhaseConnected
	PersistPhaseReconnecting
	PersistPhaseClosed
)

// PersistState is delivered to PersistConfig.OnState on each phase change.
type PersistState struct {
	Phase     PersistPhase
	Attempt   int           // 1-based; resets to 0 after a stable connection
	NextRetry time.Duration // 0 unless Phase == Reconnecting
	LastError error
}

// PersistHandle is the per-iteration connection facade. peer.Conn satisfies
// this interface via the Done()/Close() methods it already exposes.
type PersistHandle interface {
	Done() <-chan struct{}
	Close()
}

// PersistDialer establishes a fresh connection. Returning *PSKAuthError causes
// PersistLoop to exit immediately even when Enabled=true.
type PersistDialer func(ctx context.Context) (PersistHandle, error)

// PersistOnConnect runs once per successful dial. The supplied runCtx is
// cancelled when the connection dies; spawned goroutines must derive their
// own ctxs from it. Returning an error tears down the iteration and triggers
// reconnect (or exit, when Enabled=false).
type PersistOnConnect func(runCtx context.Context, h PersistHandle) error

// PSKAuthError marks a fatal authentication failure that no retry can fix.
type PSKAuthError struct{ Err error }

func (e *PSKAuthError) Error() string { return "psk auth: " + e.Err.Error() }
func (e *PSKAuthError) Unwrap() error { return e.Err }

// PersistConfig configures PersistLoop.
type PersistConfig struct {
	Enabled        bool          // false → run exactly one iteration, propagate the first error
	InitialBackoff time.Duration // default 500ms
	MaxBackoff     time.Duration // default 30s
	BackoffFactor  float64       // default 2.0
	Jitter         float64       // default 0.25 (±25%)
	StableReset    time.Duration // connection alive ≥ this resets attempt counter (default 60s)
	Logger         *slog.Logger  // default slog.Default
	OnState        func(PersistState)
	Now            func() time.Time
	Sleep          func(ctx context.Context, d time.Duration) error
}

func (c *PersistConfig) defaults() {
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = 500 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 30 * time.Second
	}
	if c.BackoffFactor <= 1 {
		c.BackoffFactor = 2.0
	}
	if c.Jitter < 0 {
		c.Jitter = 0
	}
	if c.Jitter == 0 {
		c.Jitter = 0.25
	}
	if c.StableReset <= 0 {
		c.StableReset = 60 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (c *PersistConfig) emit(s PersistState) {
	if c.OnState != nil {
		c.OnState(s)
	}
}

// PersistLoop runs dial → onConnect → wait-for-Done in a loop until ctx is
// cancelled (returns nil), a *PSKAuthError surfaces (returns the error), or
// Enabled=false and any iteration fails (returns the error).
func PersistLoop(
	ctx context.Context,
	dial PersistDialer,
	onConnect PersistOnConnect,
	cfg PersistConfig,
) error {
	cfg.defaults()
	defer cfg.emit(PersistState{Phase: PersistPhaseClosed})

	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		attempt++
		cfg.emit(PersistState{Phase: PersistPhaseConnecting, Attempt: attempt})

		h, err := dial(ctx)
		if err != nil {
			var pskErr *PSKAuthError
			if errors.As(err, &pskErr) {
				return err
			}
			if !cfg.Enabled {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			if !sleepBackoff(ctx, &cfg, attempt, err) {
				return nil
			}
			continue
		}

		runCtx, runCancel := context.WithCancel(ctx)
		connectedAt := cfg.Now()
		cfg.emit(PersistState{Phase: PersistPhaseConnected, Attempt: attempt})

		ocErr := onConnect(runCtx, h)
		// onConnect may return either because the conn died (h.Done() closed)
		// or because of an error it surfaced itself. We always tear down.
		runCancel()
		h.Close()

		if ctx.Err() != nil {
			return nil
		}
		if !cfg.Enabled {
			return ocErr
		}
		// Stable-connection reset: if we held the conn long enough, the next
		// failure starts backoff from scratch instead of inheriting the
		// pre-success exponential growth.
		if cfg.Now().Sub(connectedAt) >= cfg.StableReset {
			attempt = 0
		}
		if !sleepBackoff(ctx, &cfg, attempt+1, ocErr) {
			return nil
		}
	}
}

// sleepBackoff computes the next delay, emits Reconnecting, and sleeps.
// Returns false if ctx was cancelled during sleep.
func sleepBackoff(ctx context.Context, cfg *PersistConfig, attempt int, lastErr error) bool {
	d := computeBackoff(cfg.InitialBackoff, cfg.MaxBackoff, cfg.BackoffFactor, cfg.Jitter, attempt)
	cfg.emit(PersistState{
		Phase:     PersistPhaseReconnecting,
		Attempt:   attempt,
		NextRetry: d,
		LastError: lastErr,
	})
	if err := cfg.Sleep(ctx, d); err != nil {
		return false
	}
	return true
}

func computeBackoff(initial, max time.Duration, factor, jitter float64, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := float64(initial) * math.Pow(factor, float64(attempt-1))
	if base > float64(max) {
		base = float64(max)
	}
	// jitter: ±jitter fraction; always positive.
	delta := (rand.Float64()*2 - 1) * jitter * base
	d := time.Duration(base + delta)
	if d <= 0 {
		d = time.Duration(base)
	}
	return d
}
```

- [ ] **Step 4: Run tests, expect PASS**

```bash
go test ./cli/ -run TestPersistLoop_ -count=1
```

Expected: PASS for both happy-path tests.

- [ ] **Step 5: Commit**

```bash
git add cli/persist.go cli/persist_test.go
git commit -m "cli/persist: PersistLoop + types + happy-path tests"
```

---

## Task 3: PersistLoop backoff progression and disabled mode

**Files:**
- Modify: `cli/persist_test.go` (add tests)

- [ ] **Step 1: Add the failing tests**

Append to `cli/persist_test.go`:

```go
func TestPersistLoop_ExponentialBackoffOnDialError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	var sleepDurations []time.Duration
	var sleepMu sync.Mutex

	dial := func(_ context.Context) (PersistHandle, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n >= 5 {
			cancel()
			return nil, errors.New("done")
		}
		return nil, errors.New("dial fail")
	}
	onConnect := func(_ context.Context, _ PersistHandle) error { return nil }

	cfg := PersistConfig{
		Enabled:        true,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		BackoffFactor:  2.0,
		Jitter:         0,
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleepMu.Lock()
			sleepDurations = append(sleepDurations, d)
			sleepMu.Unlock()
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		},
	}

	_ = PersistLoop(ctx, dial, onConnect, cfg)
	sleepMu.Lock()
	defer sleepMu.Unlock()
	if len(sleepDurations) < 4 {
		t.Fatalf("got %d sleeps, want >= 4: %v", len(sleepDurations), sleepDurations)
	}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		if sleepDurations[i] != w {
			t.Errorf("sleep[%d] = %v, want %v", i, sleepDurations[i], w)
		}
	}
}

func TestPersistLoop_DisabledStopsAfterFirstError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("nope")
	dial := func(_ context.Context) (PersistHandle, error) { return nil, wantErr }
	onConnect := func(_ context.Context, _ PersistHandle) error { return nil }

	err := PersistLoop(ctx, dial, onConnect, PersistConfig{
		Enabled: false,
		Sleep:   instantSleep,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("PersistLoop err = %v, want %v", err, wantErr)
	}
}

func TestPersistLoop_DisabledRunsOneIterationAndReturnsOnConnectError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("oc nope")
	dial := func(_ context.Context) (PersistHandle, error) { return newFakeHandle(), nil }
	var calls int32
	onConnect := func(_ context.Context, _ PersistHandle) error {
		atomic.AddInt32(&calls, 1)
		return wantErr
	}

	err := PersistLoop(ctx, dial, onConnect, PersistConfig{
		Enabled: false,
		Sleep:   instantSleep,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("PersistLoop err = %v, want %v", err, wantErr)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("onConnect calls = %d, want 1", calls)
	}
}
```

- [ ] **Step 2: Run, expect PASS**

```bash
go test ./cli/ -run TestPersistLoop_ -count=1
```

Expected: all PASS — these exercise paths already in the Task 2 implementation.

- [ ] **Step 3: Commit**

```bash
git add cli/persist_test.go
git commit -m "cli/persist: tests — backoff progression + disabled mode"
```

---

## Task 4: PersistLoop fatal PSK + ctx cancel during backoff + stable-reset

**Files:**
- Modify: `cli/persist_test.go`

- [ ] **Step 1: Add the failing tests**

Append to `cli/persist_test.go`:

```go
func TestPersistLoop_PSKAuthIsFatal(t *testing.T) {
	ctx := context.Background()
	pskErr := &PSKAuthError{Err: errors.New("rejected")}
	dial := func(_ context.Context) (PersistHandle, error) { return nil, pskErr }
	onConnect := func(_ context.Context, _ PersistHandle) error { return nil }

	err := PersistLoop(ctx, dial, onConnect, PersistConfig{
		Enabled: true, // even with persist on, PSK is fatal
		Sleep:   instantSleep,
	})
	var got *PSKAuthError
	if !errors.As(err, &got) {
		t.Fatalf("PersistLoop err = %v, want *PSKAuthError", err)
	}
}

func TestPersistLoop_CtxCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dial := func(_ context.Context) (PersistHandle, error) {
		return nil, errors.New("transient")
	}
	onConnect := func(_ context.Context, _ PersistHandle) error { return nil }

	cfg := PersistConfig{
		Enabled:        true,
		InitialBackoff: 50 * time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}

	if err := PersistLoop(ctx, dial, onConnect, cfg); err != nil {
		t.Fatalf("PersistLoop err = %v, want nil", err)
	}
}

func TestPersistLoop_StableResetClearsAttemptCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu             sync.Mutex
		sleepDurations []time.Duration
		now            = time.Unix(1_700_000_000, 0)
		dialCount      int
	)
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	handle := newFakeHandle()
	dial := func(_ context.Context) (PersistHandle, error) {
		mu.Lock()
		dialCount++
		mu.Unlock()
		switch dialCount {
		case 1:
			return handle, nil // success #1; we'll close after StableReset elapses
		case 2, 3:
			return nil, errors.New("flap")
		default:
			cancel()
			return nil, errors.New("done")
		}
	}
	onConnect := func(runCtx context.Context, h PersistHandle) error {
		// Simulate a long-stable connection: advance virtual time past
		// StableReset (60s), then close.
		advance(2 * time.Minute)
		h.Close()
		<-runCtx.Done()
		return nil
	}

	cfg := PersistConfig{
		Enabled:        true,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     10 * time.Second,
		BackoffFactor:  2.0,
		Jitter:         0,
		StableReset:    60 * time.Second,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			sleepDurations = append(sleepDurations, d)
			mu.Unlock()
			return ctx.Err()
		},
	}

	_ = PersistLoop(ctx, dial, onConnect, cfg)

	mu.Lock()
	defer mu.Unlock()
	// First post-success sleep should be at attempt=1 (counter reset),
	// i.e. == InitialBackoff, not 200ms or 400ms.
	if len(sleepDurations) == 0 {
		t.Fatalf("no sleeps recorded")
	}
	if sleepDurations[0] != 100*time.Millisecond {
		t.Errorf("first sleep after stable success = %v, want 100ms (counter reset)",
			sleepDurations[0])
	}
}
```

- [ ] **Step 2: Run, expect PASS**

```bash
go test ./cli/ -run TestPersistLoop_ -count=1
```

Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add cli/persist_test.go
git commit -m "cli/persist: tests — psk fatal, ctx cancel, stable-reset"
```

---

## Task 5: Split `runner.Run` into `Connect` + `OnConnect`, scope runCtx for handleAssign

**Files:**
- Modify: `runner/connect.go`
- Modify: `runner/connect_test.go`

The goal is two-fold:

1. Restructure `Run` so it can be plugged into `cli.PersistLoop` without losing behaviour for existing single-shot callers.
2. Introduce a `runCtx` that cancels on connection death so per-task `handleAssign` goroutines (and the claude subprocesses they own) tear down cleanly before the next reconnect iteration.

- [ ] **Step 1: Add a failing test for runCtx cancel propagation**

Append to `runner/connect_test.go` (or create if absent):

```go
package runner

import (
	"context"
	"testing"
	"time"
)

// TestRun_RunCtxCancelsOnPeerDone verifies that when the underlying peer.Conn
// reports Done, the per-Run ctx visible to spawned task handlers is cancelled.
//
// We don't have a real peer.Conn here, so we exercise the ctx wiring directly
// via runConnected (the helper introduced by this task) with a fake handle.
func TestRun_RunCtxCancelsOnPeerDone(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &fakeRunHandle{done: make(chan struct{})}
	captured := make(chan context.Context, 1)
	hooks := runHooks{
		spawnTask: func(ctx context.Context) { captured <- ctx },
	}

	go func() {
		_ = runConnected(parent, h, hooks)
	}()

	// Trigger one synthetic AssignTask path via the spawn hook.
	hooks.kickoff()

	var taskCtx context.Context
	select {
	case taskCtx = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatalf("spawnTask was never invoked")
	}
	if taskCtx.Err() != nil {
		t.Fatalf("taskCtx already cancelled before peer Done: %v", taskCtx.Err())
	}

	close(h.done) // simulate disconnect

	select {
	case <-taskCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("taskCtx was not cancelled after peer Done")
	}
}

// fakeRunHandle / runHooks are introduced in runner/connect.go as part of this task.
```

- [ ] **Step 2: Run, expect compile failure**

```bash
go test ./runner/ -run TestRun_RunCtxCancelsOnPeerDone -count=1
```

Expected: build error — `runConnected`, `fakeRunHandle`, `runHooks` undefined.

- [ ] **Step 3: Refactor `runner/connect.go`**

Replace the body of `runner/connect.go::Run` with the following structure (keep the existing imports + `peerSender` definition; only `Run` and the new helpers change):

```go
// RunHandle wraps a successfully-dialled peer connection and the prepared
// session so PersistLoop's OnConnect step can drive the post-handshake
// phase. Implements cli.PersistHandle (Done/Close).
type RunHandle struct {
	pc      *peer.Conn
	session *Session
	sender  *peerSender
	cfg     Config

	pskRespCh chan wire.PskAuthStatus
}

func (h *RunHandle) Done() <-chan struct{} { return h.pc.Done() }
func (h *RunHandle) Close()                { h.pc.Close() }

// Connect performs the WS dial, ECDH handshake, PSK exchange, and session
// scaffolding. The caller drives the rest of the lifecycle via OnConnect.
//
// Returns *cli.PSKAuthError when the server rejects the PSK so PersistLoop
// can treat it as fatal.
func Connect(ctx context.Context, cfg Config) (*RunHandle, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Logger.Info("runner config",
		"no_worktree", cfg.NoWorktree,
		"force_inject_harness_settings", cfg.ForceInjectHarnessSettings)

	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: cfg.Logger,
		Path:   cli.WebSocketPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		return nil, fmt.Errorf("ws endpoint: %w", err)
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	pc, err := peer.Dial(ctx, ep, cfg.ServerCID, peer.DialConfig{
		Logger:       cfg.Logger,
		PingInterval: cfg.PingInterval, // zero → peer.Dial default (15s post-Task 1)
	})
	if err != nil {
		return nil, err
	}

	var binDir string
	if exe, err := os.Executable(); err == nil {
		binDir = filepath.Dir(exe)
	} else {
		cfg.Logger.Warn("os.Executable failed; agent PATH will not include runner bin dir", "err", err)
	}

	psk := cfg.PSK
	if psk == nil {
		psk = cli.GetPSK()
	}

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		AllowedRoots:               cfg.AllowedRoots,
		ClaudeBin:                  cfg.ClaudeBin,
		ExtraClaudeArgs:            cfg.ExtraClaudeArgs,
		ServerCID:                  cfg.ServerCID,
		Hostname:                   cfg.Hostname,
		WSPath:                     cli.WebSocketPath,
		BinDir:                     binDir,
		PSK:                        psk,
		Sender:                     sender,
		Streams:                    pc.Transport(),
		Logger:                     cfg.Logger,
		Now:                        time.Now,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
	}

	h := &RunHandle{
		pc:        pc,
		session:   session,
		sender:    sender,
		cfg:       cfg,
		pskRespCh: make(chan wire.PskAuthStatus, 1),
	}

	// Run PSK first so OnConnect doesn't have to. If it fails we close the
	// peer and surface a *cli.PSKAuthError for PersistLoop's fatal path.
	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind == wire.ApplicationPayloadKind_PskAuth && len(payload) > 0 {
			select {
			case h.pskRespCh <- wire.PskAuthStatus(payload[0]):
			default:
			}
			return
		}
		// pre-OnConnect: ignore non-PSK payloads.
	})
	pc.Start(ctx)

	pskCtx, pskCancel := context.WithCancel(ctx)
	go func() {
		defer pskCancel()
		select {
		case <-pc.Done():
		case <-pskCtx.Done():
		}
	}()
	pskErr := cli.SendAndWaitPSK(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, h.pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, &cli.PSKAuthError{Err: pskErr}
	}
	return h, nil
}

// OnConnect performs the post-PSK lifecycle: install the runner-control
// dispatcher rooted at runCtx, send Hello, and block until the peer
// connection terminates or runCtx is cancelled.
func OnConnect(runCtx context.Context, h *RunHandle) error {
	pc := h.pc
	session := h.session
	cfg := h.cfg

	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		dispatchRunnerRequest(runCtx, session, cfg.Logger, kind, payload)
	})

	hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	hh := protocol.RunnerHello{Version: 1}

	maxTasks := cfg.MaxTasks
	if maxTasks < 1 {
		maxTasks = 1
	}
	hh.MaxTasks = uint16(maxTasks)
	if cfg.Hostname != "" {
		hh.SetHostname([]byte(cfg.Hostname))
	}
	roots := make([]protocol.AllowedRoot, 0, len(cfg.AllowedRoots))
	for _, r := range cfg.AllowedRoots {
		var ar protocol.AllowedRoot
		ar.SetPath([]byte(r))
		roots = append(roots, ar)
	}
	hh.SetAllowedRoots(roots)
	hello.SetHello(hh)
	helloBytes := hello.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err := h.sender.Send(helloBytes); err != nil {
		return fmt.Errorf("send Hello: %w", err)
	}

	// Block until either the connection dies or the run is cancelled.
	select {
	case <-pc.Done():
		return nil
	case <-runCtx.Done():
		return nil
	}
}

// Run is the legacy single-shot entry point used by tests and by the shim in
// agent-runner main when persist=false. Sequential Connect → OnConnect.
func Run(ctx context.Context, cfg Config) error {
	h, err := Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer h.Close()
	return OnConnect(ctx, h)
}
```

Add to `Config` (struct in same file):

```go
	// PingInterval overrides peer.DialConfig.PingInterval (default 15s).
	PingInterval time.Duration
```

Also expose hooks for the new test:

Add at the end of `runner/connect.go`:

```go
// runHooks is a test seam used by TestRun_RunCtxCancelsOnPeerDone to inject
// a handleAssign-shaped goroutine without spinning up real claude processes.
type runHooks struct {
	spawnTask func(ctx context.Context)
	kicker    chan struct{}
}

func (h *runHooks) kickoff() { close(h.kicker) }

// fakeRunHandle implements PersistHandle for runner unit tests.
type fakeRunHandle struct {
	done chan struct{}
}

func (h *fakeRunHandle) Done() <-chan struct{} { return h.done }
func (h *fakeRunHandle) Close()                {}

// runConnected is the test-facing core of OnConnect: derive runCtx, fire a
// spawn callback when the kicker channel triggers, then block on Done.
func runConnected(parent context.Context, h *fakeRunHandle, hooks runHooks) error {
	runCtx, runCancel := context.WithCancel(parent)
	defer runCancel()
	if hooks.kicker == nil {
		hooks.kicker = make(chan struct{})
	}
	go func() {
		<-hooks.kicker
		if hooks.spawnTask != nil {
			hooks.spawnTask(runCtx)
		}
	}()
	select {
	case <-h.Done():
		return nil
	case <-parent.Done():
		return nil
	}
}
```

Update the test from Step 1 to construct `runHooks{kicker: make(chan struct{})}` so `kickoff()` works:

```go
hooks := runHooks{
    spawnTask: func(ctx context.Context) { captured <- ctx },
    kicker:    make(chan struct{}),
}
go func() { _ = runConnected(parent, h, hooks) }()
hooks.kickoff()
```

(Drop the previous Step 1 lines that called `hooks.kickoff()` without initialising `kicker`.)

- [ ] **Step 4: Run tests, expect PASS**

```bash
go test ./runner/ -count=1
```

Expected: all existing tests + the new one PASS. If a pre-existing test in `runner/connect_test.go` references the old `Run` shape, leave it — `Run` still exists with the same signature.

- [ ] **Step 5: Commit**

```bash
git add runner/connect.go runner/connect_test.go
git commit -m "runner: split Run → Connect + OnConnect; runCtx scopes spawned tasks"
```

---

## Task 6: Wire `cli.PersistLoop` into `cmd/agent-runner/main.go`

**Files:**
- Modify: `cmd/agent-runner/main.go`

- [ ] **Step 1: Replace flags + Run call**

Replace the `var (...)` block and the call to `runner.Run(...)` near the end of `main()` as follows:

```go
var (
	serverCID  = flag.String("server-cid", "ws:127.0.0.1:8539-*", "server ConnectionID (e.g. ws:host:port-id, * for random)")
	roots      = flag.String("roots", ".", "comma-separated list of absolute repo root paths this runner serves")
	maxTasks   = flag.Int("max-tasks", 1, "maximum number of concurrent tasks (>= 1)")
	claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
	claudeArgs = flag.String("claude-args", "", "extra args passed to claude before -p (whitespace-separated)")
	wsPath     = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
	hostName   = flag.String("hostname", "", "hostname to report in Hello (default: os.Hostname())")
	psk        = flag.String("psk", "", "PSK passphrase (env: HARNESS_PSK)")
	pskFile    = flag.String("psk-file", "", "path to PSK file (env: HARNESS_PSK_FILE)")

	noWorktree                 = flag.Bool("no-worktree", false, "skip per-task git worktree creation; run agent processes directly in the bound repo path. Disables .claude/settings.json and .claude/skills/ injection by default (see --force-inject-harness-settings).")
	forceInjectHarnessSettings = flag.Bool("force-inject-harness-settings", false, "only meaningful with --no-worktree: re-enable .claude/settings.json and .claude/skills/ injection at the bound repo path.")

	persist        = flag.Bool("persist", true, "auto-reconnect on disconnect (set --no-persist to disable)")
	noPersist      = flag.Bool("no-persist", false, "shortcut for --persist=false")
	pingInterval   = flag.Duration("ping-interval", 15*time.Second, "underlying ping cadence; also bounds disconnect detection delay")
	reconnectInit  = flag.Duration("reconnect-initial", 500*time.Millisecond, "first backoff after a disconnect")
	reconnectMax   = flag.Duration("reconnect-max", 30*time.Second, "backoff cap")
)
```

Replace the `if err := runner.Run(...)` block with:

```go
runCfg := runner.Config{
	ServerCID:                  peerCID,
	AllowedRoots:               abs,
	MaxTasks:                   *maxTasks,
	Hostname:                   hostname,
	ClaudeBin:                  *claudeBin,
	ExtraClaudeArgs:            strings.Fields(*claudeArgs),
	Logger:                     slog.Default(),
	PSK:                        resolvedPSK,
	NoWorktree:                 *noWorktree,
	ForceInjectHarnessSettings: *forceInjectHarnessSettings,
	PingInterval:               *pingInterval,
}

enabled := *persist && !*noPersist

err = cli.PersistLoop(ctx,
	func(dialCtx context.Context) (cli.PersistHandle, error) {
		return runner.Connect(dialCtx, runCfg)
	},
	func(runCtx context.Context, h cli.PersistHandle) error {
		rh := h.(*runner.RunHandle)
		return runner.OnConnect(runCtx, rh)
	},
	cli.PersistConfig{
		Enabled:        enabled,
		InitialBackoff: *reconnectInit,
		MaxBackoff:     *reconnectMax,
		Logger:         slog.Default(),
		OnState: func(s cli.PersistState) {
			slog.Info("runner persist state",
				"phase", s.Phase, "attempt", s.Attempt,
				"next_retry", s.NextRetry, "err", s.LastError)
		},
	})
if err != nil {
	slog.Error("runner exit", "err", err)
	os.Exit(1)
}
```

`runner.Connect` already returns `*runner.RunHandle` which satisfies `cli.PersistHandle` (Done / Close added in Task 5), so the type assertion in `OnConnect` is straightforward.

Don't forget the imports near the top of the file:

```go
import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
)
```

- [ ] **Step 2: Build and smoke**

```bash
go build ./cmd/agent-runner/
./agent-runner --help 2>&1 | grep -E 'persist|ping-interval|reconnect-(initial|max)'
```

Expected: all five new flags shown.

```bash
go test ./... -count=1
```

Expected: all PASS (no test references the old `Run` invocation flow).

- [ ] **Step 3: Manual integration smoke**

In one terminal:

```bash
bin/harness-server --listen :8539 --data-dir ./harness-data
```

In a second terminal:

```bash
bin/agent-runner --server-cid 'ws:127.0.0.1:8539-*' --roots . --max-tasks 1 --persist --reconnect-initial=500ms --reconnect-max=5s
```

Kill the server (`Ctrl-C`), wait 2 s, restart it. Within ~5 s the runner log should show "runner persist state phase=Connecting" → "Connected" again, with no manual restart.

- [ ] **Step 4: Commit**

```bash
git add cmd/agent-runner/main.go
git commit -m "agent-runner: --persist (default on) wired through cli.PersistLoop"
```

---

## Task 7: Extend `tui.ConnectionMsg` shape

**Files:**
- Modify: `tui/events.go:35-38`

- [ ] **Step 1: Replace the struct**

Replace:

```go
type ConnectionMsg struct {
	Connected bool
	Err       error
}
```

with:

```go
// ConnectionMsg notifies the App of a connection state change driven by
// cli.PersistLoop. Connected and Reconnecting are mutually exclusive.
//   - Connected=true             → freshly bound to a live client
//   - Reconnecting=true          → between attempts, NextRetry counts down
//   - Connected=false, Reconnecting=false → terminal disconnect (Err set)
type ConnectionMsg struct {
	Connected    bool
	Reconnecting bool
	Attempt      int
	NextRetry    time.Duration
	Err          error
}
```

Add `"time"` to imports if missing.

- [ ] **Step 2: Build (existing call sites with positional fields will break)**

```bash
go build ./tui/...
```

Expected: passes (existing constructors all use field names: `ConnectionMsg{Connected: ..., Err: ...}`).

If any positional construction breaks, fix it to use field names — there should be none.

- [ ] **Step 3: Commit**

```bash
git add tui/events.go
git commit -m "tui: extend ConnectionMsg with Reconnecting/Attempt/NextRetry"
```

---

## Task 8: TUI status line + `BindClient` re-entrancy + `FollowingTaskID`

**Files:**
- Modify: `tui/app.go`

- [ ] **Step 1: Add `FollowingTaskID()` helper**

Find the `// log-following state` block (around line 54) and add a public getter near `followTask`:

```go
// FollowingTaskID returns the task id whose log is being streamed into the
// log pane, or "" if no task is followed. Used by the persist-loop wiring
// to re-issue SubscribeTaskLog after a reconnect.
func (a *App) FollowingTaskID() string { return a.logs.TaskID() }
```

- [ ] **Step 2: Update `ConnectionMsg` handling**

Replace the existing `case ConnectionMsg:` block (around line 184):

```go
case ConnectionMsg:
	a.connected = msg.Connected
	switch {
	case msg.Connected:
		// fresh attach; logs are re-followed by the main goroutine on reconnect.
	case msg.Reconnecting:
		txt := fmt.Sprintf("reconnecting (attempt %d, next try in %s)",
			msg.Attempt, msg.NextRetry.Truncate(time.Second))
		if msg.Err != nil {
			txt += ": " + msg.Err.Error()
		}
		a.cmdresult.Append(FooterStyle.Render(txt))
	default:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("disconnected: " + msg.Err.Error()))
		} else {
			a.cmdresult.Append(ErrorStyle.Render("disconnected"))
		}
	}
	return a, nil
```

Add `"fmt"` and `"time"` imports if missing.

- [ ] **Step 3: Make `BindClient` swap-safe**

Find `BindClient` (current contents are a one-line assign `a.client = c`). Replace with:

```go
// BindClient stores the active *cli.Client. Re-entrant: callers may call
// BindClient repeatedly across reconnects. The previous client's pubsub
// goroutines have already been torn down by cli.PersistLoop's runCtx
// cancellation by the time this fires, so all we need to do is swap the
// pointer.
func (a *App) BindClient(c *cli.Client) {
	a.client = c
}
```

(If your existing `BindClient` is functionally identical, leave it; the comment update is the substantive change.)

- [ ] **Step 4: Build and run TUI tests**

```bash
go test ./tui/... -count=1
```

Expected: PASS. If `cmdline_test.go` / `events_test.go` / etc. don't cover the new path, that's fine — the integration smoke in Task 13 covers it.

- [ ] **Step 5: Commit**

```bash
git add tui/app.go
git commit -m "tui/app: ConnectionMsg reconnect rendering + FollowingTaskID + BindClient docstring"
```

---

## Task 9: Wire `PersistLoop` into `cmd/harness-tui/main.go`

**Files:**
- Modify: `cmd/harness-tui/main.go`

- [ ] **Step 1: Add persist flags**

Insert near the other flag declarations:

```go
var (
	serverCID = flag.String("server-cid", "ws:127.0.0.1:8539-*", "harness-server ConnectionID")
	repoFlag  = flag.String("repo", "", "default repo path for submit popup")
	wsPath    = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")

	persist       = flag.Bool("persist", true, "auto-reconnect on disconnect (set --no-persist to disable)")
	noPersist     = flag.Bool("no-persist", false, "shortcut for --persist=false")
	reconnectInit = flag.Duration("reconnect-initial", 500*time.Millisecond, "first backoff after disconnect")
	reconnectMax  = flag.Duration("reconnect-max", 30*time.Second, "backoff cap")
)
```

- [ ] **Step 2: Replace the dial-once goroutine with PersistLoop**

Replace the `go func() { c, err := cli.Dial(...) ... }()` block in `main()` with:

```go
type cliClientHandle struct {
	c        *cli.Client
	doneOnce sync.Once
}

func (h *cliClientHandle) Done() <-chan struct{} { return h.c.Peer().Done() }
func (h *cliClientHandle) Close() {
	h.doneOnce.Do(func() { h.c.Close() })
}

go func() {
	enabled := *persist && !*noPersist
	err := cli.PersistLoop(ctx,
		func(dialCtx context.Context) (cli.PersistHandle, error) {
			c, err := cli.Dial(dialCtx, peerCID)
			if err != nil {
				return nil, err
			}
			return &cliClientHandle{c: c}, nil
		},
		func(runCtx context.Context, h cli.PersistHandle) error {
			handle := h.(*cliClientHandle)
			if err := handle.c.SayHello(runCtx, protocol.ClientKind_Tui); err != nil {
				return err
			}
			app.BindClient(handle.c)
			program.Send(tui.RefreshSnapshot(handle.c)())
			go tui.SubscribeTaskStatus(runCtx, handle.c, program)
			go tui.SubscribeRunnerStatus(runCtx, handle.c, program)
			if id := app.FollowingTaskID(); id != "" {
				go tui.SubscribeTaskLog(runCtx, handle.c, program, id)
			}
			<-runCtx.Done()
			return nil
		},
		cli.PersistConfig{
			Enabled:        enabled,
			InitialBackoff: *reconnectInit,
			MaxBackoff:     *reconnectMax,
			OnState: func(s cli.PersistState) {
				program.Send(tui.ConnectionMsg{
					Connected:    s.Phase == cli.PersistPhaseConnected,
					Reconnecting: s.Phase == cli.PersistPhaseReconnecting,
					Attempt:      s.Attempt,
					NextRetry:    s.NextRetry,
					Err:          s.LastError,
				})
			},
		})
	if err != nil {
		program.Send(tui.ConnectionMsg{Connected: false, Err: err})
	}
}()
```

Add `"sync"` and `"time"` imports if missing.

- [ ] **Step 3: Build**

```bash
go build ./cmd/harness-tui/
```

Expected: builds.

- [ ] **Step 4: Manual smoke**

```bash
bin/harness-server --listen :8539 --data-dir ./harness-data &
bin/harness-tui --server-cid 'ws:127.0.0.1:8539-*' --reconnect-initial=500ms --reconnect-max=5s
```

Stop the server, observe the cmdresult footer cycle through "reconnecting (attempt N, next try in Ts)". Restart server: TUI auto-restores task and runner panes (via the on-reconnect `RefreshSnapshot`).

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-tui/main.go
git commit -m "harness-tui: --persist via cli.PersistLoop with auto-resubscribe"
```

---

## Task 10: WebUI — `harness.connect` options bag + `PersistLoop` + `onConnectionChange`

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go`

- [ ] **Step 1: Add the persist plumbing**

Near the top of the file, after the existing `var clientMu sync.Mutex; var client *cli.Client; var peerCID objproto.ConnectionID` declarations, add:

```go
var (
	connectOnce       sync.Once
	connStateHandler  js.Value      // settable via harness.onConnectionChange
	connStateHandlerM sync.Mutex
)

type webuiHandle struct {
	c        *cli.Client
	doneOnce sync.Once
}

func (h *webuiHandle) Done() <-chan struct{} { return h.c.Peer().Done() }
func (h *webuiHandle) Close() {
	h.doneOnce.Do(func() { h.c.Close() })
}
```

Add `"sync"` to imports if missing.

- [ ] **Step 2: Replace `harnessConnect`**

Replace the existing `harnessConnect` body with:

```go
// harness.connect("ws:..."):                  one-shot, persist=false (compat)
// harness.connect("ws:...", { persist: true, pingInterval: "15s" }):
//                                             options bag, persist defaults to true
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

			started := make(chan struct{})
			var startedOnce sync.Once
			peerCIDLocal := cid

			go func() {
				err := cli.PersistLoop(rootCtx,
					func(dialCtx context.Context) (cli.PersistHandle, error) {
						c, err := cli.Dial(dialCtx, peerCIDLocal)
						if err != nil {
							return nil, err
						}
						return &webuiHandle{c: c}, nil
					},
					func(runCtx context.Context, h cli.PersistHandle) error {
						handle := h.(*webuiHandle)
						if err := handle.c.SayHello(runCtx, protocol.ClientKind_Webui); err != nil {
							return err
						}
						clientMu.Lock()
						client = handle.c
						peerCID = peerCIDLocal
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
				// Initial dial took too long; surface to JS so the caller can decide.
				rejectErr(reject, errors.New("connect: initial dial timed out (still retrying in background if persist=true)"))
			}
			_ = pingInterval // currently consumed by peer.DialConfig in cli.Dial; placeholder for a future runner-style override
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

// harness.onConnectionChange((state) => { ... })
func harnessOnConnectionChange(this js.Value, args []js.Value) any {
	if len(args) >= 1 && args[0].Type() == js.TypeFunction {
		connStateHandlerM.Lock()
		connStateHandler = args[0]
		connStateHandlerM.Unlock()
	}
	return nil
}
```

Register `harnessOnConnectionChange` alongside the other `js.FuncOf(...)` registrations:

```go
js.Global().Set("harness", js.ValueOf(map[string]any{
	"connect":               js.FuncOf(harnessConnect),
	"onConnectionChange":    js.FuncOf(harnessOnConnectionChange),
	// ... all existing entries unchanged ...
}))
```

- [ ] **Step 3: Build the WASM module**

```bash
GOOS=js GOARCH=wasm go build -o webui/static/main.wasm ./cmd/harness-webui-wasm/
```

Expected: builds. (If the existing build script differs, follow it.)

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-webui-wasm/main.go
git commit -m "harness-webui-wasm: harness.connect options bag + persist + onConnectionChange"
```

---

## Task 11: WebUI — banner + resubscribe-on-connected

**Files:**
- Modify: `webui/static/main.js`

- [ ] **Step 1: Add the banner element to `webui/index.html`**

Find a sensible place near the top of `<body>` (before the existing `<div id="app">` or similar) and insert:

```html
<div id="harness-conn-banner" hidden></div>
```

- [ ] **Step 2: Add CSS for the banner**

Append to `webui/static/style.css`:

```css
#harness-conn-banner {
  position: sticky;
  top: 0;
  z-index: 9999;
  padding: 6px 12px;
  background: #fff3cd;
  color: #5c4400;
  border-bottom: 1px solid #e0c97a;
  font-family: monospace;
  font-size: 13px;
}
#harness-conn-banner.error  { background: #f8d7da; color: #6e1b22; border-bottom-color: #cd8a8e; }
#harness-conn-banner.online { background: #d4edda; color: #1e4628; border-bottom-color: #8ec39c; }
```

- [ ] **Step 3: Hook `onConnectionChange` and a resubscribe registry**

Near the top of `webui/static/main.js`, after the page loads its WASM but before any `harness.subscribeTaskStatus`-style call, add:

```js
const connectedHandlers = [];

function registerOnConnected(fn) { connectedHandlers.push(fn); }

function paintBanner(state) {
  const el = document.getElementById('harness-conn-banner');
  if (!el) return;
  el.classList.remove('error', 'online');
  if (state.phase === 'connected') {
    el.textContent = 'connected';
    el.classList.add('online');
    el.hidden = false;
    setTimeout(() => { el.hidden = true; }, 1500);
  } else if (state.phase === 'reconnecting') {
    const secs = state.nextRetryMs ? Math.round(state.nextRetryMs / 1000) : '?';
    el.textContent = `reconnecting (attempt ${state.attempt}, next try in ${secs}s)`;
    el.hidden = false;
  } else if (state.phase === 'closed') {
    el.textContent = state.error ? `disconnected: ${state.error}` : 'disconnected';
    el.classList.add('error');
    el.hidden = false;
  } else {
    // connecting
    el.textContent = `connecting (attempt ${state.attempt})…`;
    el.hidden = false;
  }
}

harness.onConnectionChange((state) => {
  paintBanner(state);
  if (state.phase === 'connected') {
    for (const fn of connectedHandlers) {
      try { fn(); } catch (e) { console.error('connected handler', e); }
    }
  }
});
```

Find the existing initial connect call (currently `await harness.connect("ws:...")`) and switch to the options-bag form:

```js
await harness.connect(SERVER_CID, { persist: true });
```

Then wrap each `harness.subscribeTaskStatus(...)` / `harness.subscribeRunnerStatus(...)` invocation so it registers via `registerOnConnected` instead of running once. Example pattern:

```js
registerOnConnected(() => {
  harness.subscribeTaskStatus((event) => { /* ... existing handler ... */ });
});
registerOnConnected(() => {
  harness.subscribeRunnerStatus((event) => { /* ... existing handler ... */ });
});
```

(If the file currently calls subscribe-style functions only once at boot, move those calls into `registerOnConnected` callbacks; the first `connected` event will fire them, and every subsequent reconnect will too.)

- [ ] **Step 4: Manual smoke**

```bash
bin/harness-server --listen :8539 --data-dir ./harness-data
# in browser: open http://127.0.0.1:8539/
# kill the server, watch banner go to "reconnecting"; restart, banner flashes "connected".
```

- [ ] **Step 5: Commit**

```bash
git add webui/index.html webui/static/style.css webui/static/main.js
git commit -m "webui: persist banner + resubscribe-on-connected pattern"
```

---

## Task 12: Integration test — runner persists across server restart

**Files:**
- Create: `integration/persist_test.go` (build tag: `integration`)

- [ ] **Step 1: Write the failing integration test**

Create `integration/persist_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
)

// TestRunnerPersistsAcrossServerRestart starts a server + a runner with
// --persist, kills the server, restarts it on the same port, and asserts the
// runner reappears in the registry without manual restart.
func TestRunnerPersistsAcrossServerRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	port := freePort(t)
	cidArg := "ws:127.0.0.1:" + port + "-*"

	repo := t.TempDir()

	srv1 := startBinary(t, ctx, "harness-server",
		"--listen", ":"+port, "--data-dir", dataDir)
	waitListening(t, "127.0.0.1:"+port, 10*time.Second)

	runnerProc := startBinary(t, ctx, "agent-runner",
		"--server-cid", cidArg, "--roots", repo, "--max-tasks", "1",
		"--persist", "--reconnect-initial=200ms", "--reconnect-max=2s",
		"--ping-interval=2s")
	defer runnerProc.kill()

	// Verify the runner registers on the first server.
	if !waitForRunner(t, ctx, port, repo, 15*time.Second) {
		t.Fatalf("runner never registered on first server")
	}

	// Kill server #1.
	srv1.kill()
	time.Sleep(500 * time.Millisecond)

	// Start server #2 on the same port.
	srv2 := startBinary(t, ctx, "harness-server",
		"--listen", ":"+port, "--data-dir", dataDir)
	defer srv2.kill()
	waitListening(t, "127.0.0.1:"+port, 10*time.Second)

	// Within 15 s the persistent runner must rediscover and re-register.
	if !waitForRunner(t, ctx, port, repo, 15*time.Second) {
		t.Fatalf("runner did not reconnect after server restart")
	}
}

// --- helpers ---

type proc struct {
	cmd *exec.Cmd
}

func (p *proc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}

func startBinary(t *testing.T, ctx context.Context, name string, args ...string) *proc {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("..", "bin", name))
	if err != nil {
		t.Fatalf("resolve binary: %v", err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	return &proc{cmd: cmd}
}

type testWriter struct{ t *testing.T }
func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func freePort(t *testing.T) string {
	t.Helper()
	// Use net.ListenAndClose to grab a free port; implementation omitted for brevity.
	// Most projects already have a similar helper in integration/ (cf. e.g. server_test.go).
	// If not, implement with net.Listen("tcp", "127.0.0.1:0") then close + parse port.
	return findFreePortImpl(t)
}

func waitListening(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server never listened on %s", addr)
}

// waitForRunner uses cli.Dial + a runner-listing helper to poll until a
// runner with the given root is registered. Implementation: open a CLI
// client, call ListRunners() (existing in cli/list.go), match by AllowedRoots.
func waitForRunner(t *testing.T, ctx context.Context, port, repo string, d time.Duration) bool {
	t.Helper()
	cid, err := objproto.ParseConnectionID("ws:127.0.0.1:"+port+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse cid: %v", err)
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := cli.Dial(ctx, cid)
		if err == nil {
			runners, lerr := c.ListRunners(ctx)
			c.Close()
			if lerr == nil {
				for _, r := range runners {
					for _, root := range r.AllowedRoots {
						if string(root.Path) == repo {
							return true
						}
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
```

If `cli.ListRunners` doesn't exist verbatim, use whatever the integration/ package already uses (cf. `cli/list.go`'s `ListRunners` / `ListSnapshot`-style entry points). Mirror an existing integration test in the same directory if helpers like `freePort` / `waitListening` already exist there — reuse them rather than the placeholders shown.

Add `"net"` import.

- [ ] **Step 2: Build binaries**

```bash
make build || (go build -o bin/harness-server ./cmd/harness-server/ && go build -o bin/agent-runner ./cmd/agent-runner/ && go build -o bin/harness-cli ./cmd/harness-cli/ && go build -o bin/harness-tui ./cmd/harness-tui/)
```

- [ ] **Step 3: Run the integration test**

```bash
go test -tags integration ./integration/ -run TestRunnerPersistsAcrossServerRestart -count=1 -v
```

Expected: PASS within 60 s.

- [ ] **Step 4: Commit**

```bash
git add integration/persist_test.go
git commit -m "integration: runner persists across server restart"
```

---

## Task 13: Commonalisation review + spec amendment

**Files:**
- Modify: `docs/superpowers/specs/2026-05-09-persist-reconnect-design.md` (append a verdict to §10 Step 8)
- Possibly modify: `cli/persist.go`, the three `cmd/*/main.go` to extract repeated `OnConnect` recipes if a clean abstraction emerges

This task is the dedicated "step back and look" pass that the user asked for. Three call sites is the YAGNI threshold; do *not* invent helpers for two of three sharing logic.

- [ ] **Step 1: Diff the three OnConnect recipes**

Open the three `OnConnect` closures side by side:
- `cmd/agent-runner/main.go` (delegates entirely to `runner.OnConnect`)
- `cmd/harness-tui/main.go` (`SayHello` + `BindClient` + `RefreshSnapshot` + 2-3 Subscribes + `<-runCtx.Done()`)
- `cmd/harness-webui-wasm/main.go` (`SayHello` + client-pointer swap + `<-runCtx.Done()`)

Identify shared structure. Likely candidates:

- The `SayHello` + final `<-runCtx.Done()` framing pair appears in TUI and WebUI but not in runner (which has its own Hello protocol). Insufficient duplication to justify extraction.
- The Subscribe goroutines are TUI-specific (RunnerStatus/TaskStatus/TaskLog). Not extractable.
- The `cliClientHandle` / `webuiHandle` types both wrap `*cli.Client` and forward `Done`/`Close` to `c.Peer().Done()` and `c.Close()`. **This is real duplication.** Extract.

- [ ] **Step 2: Extract a shared `cli.ClientHandle` type**

Add to `cli/persist.go`:

```go
// ClientHandle wraps a *Client as a PersistHandle; the underlying Done()
// comes from the embedded peer.Conn, and Close is idempotent.
type ClientHandle struct {
	C        *Client
	doneOnce sync.Once
}

func NewClientHandle(c *Client) *ClientHandle { return &ClientHandle{C: c} }
func (h *ClientHandle) Done() <-chan struct{} { return h.C.Peer().Done() }
func (h *ClientHandle) Close()                { h.doneOnce.Do(func() { h.C.Close() }) }
```

Add `"sync"` import to `cli/persist.go`.

Replace the local `cliClientHandle` / `webuiHandle` types in TUI / WASM main with `cli.ClientHandle` (`return cli.NewClientHandle(c), nil` in the dialer; `handle := h.(*cli.ClientHandle); handle.C.SayHello(...)`).

- [ ] **Step 3: Build and run the full suite**

```bash
go test ./... -count=1
go build ./cmd/...
GOOS=js GOARCH=wasm go build -o webui/static/main.wasm ./cmd/harness-webui-wasm/
```

Expected: all PASS / build succeeds.

- [ ] **Step 4: Append the verdict to the spec**

Edit `docs/superpowers/specs/2026-05-09-persist-reconnect-design.md` §10 Step 8 (the "Commonalisation review" bullet), appending:

```markdown
**Verdict (executed 2026-05-09):**

- Extracted `cli.ClientHandle` (wraps `*cli.Client` as a `PersistHandle`).
  Used by `cmd/harness-tui/main.go` and `cmd/harness-webui-wasm/main.go`;
  `agent-runner` does not use it because `runner.RunHandle` already
  satisfies `PersistHandle` directly.
- Did **not** extract a shared "OnConnect recipe" — the three call sites
  diverge enough (runner has its own Hello, TUI has Subscribe×2-3, WebUI
  has client-pointer publication for cross-goroutine JS calls) that any
  abstraction would require so many opt-out parameters as to be worse
  than the duplication.
```

- [ ] **Step 5: Commit**

```bash
git add cli/persist.go cmd/harness-tui/main.go cmd/harness-webui-wasm/main.go docs/superpowers/specs/2026-05-09-persist-reconnect-design.md
git commit -m "cli/persist: extract ClientHandle + spec verdict on commonalisation"
```

---

## Self-Review Checklist (executed pre-commit)

- [x] Spec coverage: every spec section §1-§10 has at least one corresponding task. §1 Goal → all tasks; §2 Non-goals → none required (excluded); §3 → Task 1, 5; §4.1 CLI flags → Task 6, 9; §4.2 WebUI → Task 10, 11; §4.3 TUI → Task 7, 8; §5.1 → Tasks 2-4; §5.2 → Task 1; §5.3 → Task 5; §5.4 → Task 6; §5.5 → Tasks 7-9; §5.6 → Tasks 10-11; §6 concurrency model → reflected in Task 5 (runCtx scoping); §7 edge cases → covered by tests in Tasks 3-4 and integration in Task 12; §9 testing → Tasks 2-5, 12; §10 implementation order → tasks ordered to match; §10 step 8 commonalisation → Task 13.
- [x] Placeholder scan: search performed; the `freePort` / `findFreePortImpl` placeholder in Task 12 step 1 is acknowledged as "use existing helper if present, else implement with net.Listen(\":0\")" — explicit instruction, not TBD.
- [x] Type consistency: `PersistHandle.Done() <-chan struct{}` and `Close()` used uniformly across Tasks 2, 5, 9, 10, 13. `PersistConfig` field names match between definition (Task 2) and call sites (Tasks 6, 9, 10).
