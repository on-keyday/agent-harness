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

func TestPersistLoop_DisabledReturnsErrConnectionClosedOnPeerDrop(t *testing.T) {
	ctx := context.Background()
	dial := func(_ context.Context) (PersistHandle, error) {
		h := newFakeHandle()
		// Simulate peer dropping the connection asynchronously.
		go func() {
			time.Sleep(5 * time.Millisecond)
			h.Close()
		}()
		return h, nil
	}
	onConnect := func(runCtx context.Context, _ PersistHandle) error {
		<-runCtx.Done()
		return nil
	}

	err := PersistLoop(ctx, dial, onConnect, PersistConfig{
		Enabled: false,
		Sleep:   instantSleep,
	})
	if !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("PersistLoop err = %v, want ErrConnectionClosed", err)
	}
}
