package server

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// closeListeners (the hold's last step) waits for the HTTP server to stop, and
// serve() leaves through the same event (a held stop exits before the root
// context is cancelled). Both must see it: with a one-shot channel whichever
// read second blocked forever — RunHoldSequence never returned on an embedded
// server. Repeated, because which side won was a race.
func TestCloseListenersReturnsWhileServeAlsoExits(t *testing.T) {
	for i := 0; i < 10; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", 18700+i)
		s := New(Config{Addr: addr})
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- s.Run(ctx) }()
		time.Sleep(150 * time.Millisecond)

		closed := make(chan struct{})
		go func() { s.closeListeners(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("round %d: closeListeners did not return", i)
		}
		select {
		case err := <-runDone:
			if err != nil {
				t.Fatalf("round %d: Run returned %v, want nil after the listeners closed", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: Run did not return after the listeners closed", i)
		}
		cancel()
	}
}
