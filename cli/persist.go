package cli

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ClientHandle wraps a *Client as a PersistHandle. Done() comes from the
// embedded peer.Conn; Close is idempotent. Used by long-lived clients (TUI,
// WebUI WASM) to plug a *cli.Client into PersistLoop without redefining the
// adapter inline. The runner has its own RunHandle (in package runner) that
// implements PersistHandle directly via *peer.Conn, so it does not use this.
type ClientHandle struct {
	C        *Client
	doneOnce sync.Once
}

// NewClientHandle wraps a *Client.
func NewClientHandle(c *Client) *ClientHandle { return &ClientHandle{C: c} }

func (h *ClientHandle) Done() <-chan struct{} { return h.C.Peer().Done() }
func (h *ClientHandle) Close()                { h.doneOnce.Do(func() { h.C.Close() }) }

// ErrConnectionClosed is returned by PersistLoop when Enabled=false and the
// peer closed the underlying connection (rather than the caller cancelling
// ctx). Callers that map this to a non-zero exit code should test with
// errors.Is(err, cli.ErrConnectionClosed).
var ErrConnectionClosed = errors.New("persist: connection closed by peer")

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

// PskRejectedError marks an EXPLICIT server rejection of the merged handshake
// (BadPsk / BadTicket / NoIdentity). A transport drop or context cancellation
// DURING the handshake returns a plain (retryable) error instead (e.g.
// context.Canceled), so a server restart that interrupts an in-flight handshake
// triggers a normal reconnect rather than killing the runner/client.
//
// An explicit rejection is fatal ONLY when it is a credential failure — see
// Retryable. Dial and runner.Connect wrap a PskRejectedError as *PSKAuthError
// (fatal) only when !Retryable(); everything else propagates as retryable.
// Code is the ONLY field on purpose. An earlier version also carried a
// human-readable `Status string`, and every construction site had to set both
// from the same resp.Status — three sites, hand-built. One of them (the runner's
// own RunnerHello handshake in runner/connect.go, easy to miss when grepping
// cli/) set Status but not Code; the zero Code read as PskAuthStatus_Ok, made
// Retryable() false, and silently restored fatal-on-wire-skew. Deriving the
// message from Code makes that whole class unrepresentable. Construct via
// NewPskRejectedError so a field can never be forgotten.
type PskRejectedError struct {
	Code protocol.PskAuthStatus
}

// NewPskRejectedError builds the error from the wire status. Use this instead of
// a struct literal: it is the single place that knows what a rejection needs.
func NewPskRejectedError(status protocol.PskAuthStatus) *PskRejectedError {
	return &PskRejectedError{Code: status}
}

func (e *PskRejectedError) Error() string { return "psk: server rejected: " + e.Code.String() }

// Retryable reports whether reconnecting could plausibly succeed.
//
// BadPsk / BadTicket are credential failures: no retry fixes a wrong PSK or an
// invalid ticket, so they stay fatal.
//
// NoIdentity is NOT a credential failure. It means the server accepted the
// binder but could not read an identity union — which psk.go long assumed could
// only be our own bug ("should not happen: we always embed a hello"). It ALSO
// happens when a version-skewed server cannot DECODE the hello we did send: e.g.
// the runner was upgraded past a wire/schema change before the server was. That
// fixes itself the moment the server is upgraded, so it MUST back off and retry.
// Treating it as fatal is what turned a restart-order mistake into a permanent
// fleet-wide wipe (2026-07-16: every runner exited within ~1s and none returned
// when the server was upgraded) — the exact failure this fatal/retryable split
// was drawn to prevent.
func (e *PskRejectedError) Retryable() bool {
	return e.Code == protocol.PskAuthStatus_NoIdentity
}

// PersistConfig configures PersistLoop.
type PersistConfig struct {
	Enabled        bool          // false → run exactly one iteration, propagate the first error
	InitialBackoff time.Duration // default 500ms
	MaxBackoff     time.Duration // default 30s
	BackoffFactor  float64       // default 2.0
	// Jitter is the ±fraction applied to each backoff sleep. Set to a positive
	// value (e.g. 0.25) for ±25% jitter; set to 0 to disable jitter
	// (deterministic backoff, useful for tests); leave negative or unset to
	// use the default 0.25.
	Jitter      float64
	StableReset time.Duration // connection alive ≥ this resets attempt counter (default 60s)
	Logger      *slog.Logger  // default slog.Default
	OnState     func(PersistState)
	Now         func() time.Time
	Sleep       func(ctx context.Context, d time.Duration) error
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
		c.Jitter = 0.25
	}
	// c.Jitter == 0 is honoured literally (no jitter, deterministic backoff).
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
				cfg.Logger.Error("persist: psk auth failed", "err", err)
				return err
			}
			if !cfg.Enabled {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			if !sleepBackoff(ctx, &cfg, attempt+1, err) {
				return nil
			}
			continue
		}

		cfg.Logger.Info("persist: connected", "attempt", attempt)

		runCtx, runCancel := context.WithCancel(ctx)
		connectedAt := cfg.Now()
		cfg.emit(PersistState{Phase: PersistPhaseConnected, Attempt: attempt})

		// peerClosed is set by the watcher when h.Done() fires first,
		// allowing Enabled=false callers to distinguish peer-drop from clean
		// cancellation.
		var peerClosed atomic.Bool

		// Run onConnect in a goroutine so we can cancel runCtx independently
		// when h.Done() fires (connection died). This lets onConnect wait on
		// runCtx.Done() to observe connection loss.
		ocErrCh := make(chan error, 1)
		go func() { ocErrCh <- onConnect(runCtx, h) }()

		// Cancel runCtx when the connection dies or the parent ctx is done.
		go func() {
			select {
			case <-h.Done():
				peerClosed.Store(true)
				runCancel()
			case <-ctx.Done():
				runCancel()
			case <-runCtx.Done():
				// onConnect returned and called runCancel already; nothing to do.
			}
		}()

		ocErr := <-ocErrCh
		// Always tear down cleanly regardless of exit path.
		runCancel()
		h.Close()

		if ctx.Err() != nil {
			return nil
		}
		if !cfg.Enabled {
			if ocErr != nil {
				return ocErr
			}
			if peerClosed.Load() {
				return ErrConnectionClosed
			}
			return nil
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
	cfg.Logger.Info("persist: reconnecting", "attempt", attempt, "next_retry", d, "err", lastErr)
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
