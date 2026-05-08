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
