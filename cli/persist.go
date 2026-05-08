package cli

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"sync/atomic"
	"time"
)

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
			if !sleepBackoff(ctx, &cfg, attempt, err) {
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
