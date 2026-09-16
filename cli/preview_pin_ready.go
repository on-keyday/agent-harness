package cli

import (
	"context"
	"sync"
)

// pinReady is the readiness of one preview pin's forward registration.
//
// It exists because a pin is entered in the registry BEFORE its
// RegisterPortForward round trip and completed after it (see OpenPreviewPin in
// preview_forward_wasm.go), while the page that uses the pin starts running the
// moment the iframe is built. A page that fetches on load — which is the normal
// shape for anything worth previewing — therefore races the registration, and
// used to be told "no live pin for this preview" when the truth was "not yet".
//
// Deliberately in a file with no build constraint, unlike everything else about
// pins: the registry is js-only, so a test for it would have to run under
// GOOS=js and `make test` runs no such thing. A rule nothing executes is not a
// rule, so the waiting lives here where `go test ./cli/` covers it.
type pinReady struct {
	done chan struct{}
	once sync.Once
	err  error // written once, under once; read only after done is closed
}

func newPinReady() *pinReady {
	return &pinReady{done: make(chan struct{})}
}

// settle records how the registration ended and releases every waiter. Only the
// FIRST call counts: a supersede racing the RPC's return would otherwise
// rewrite an answer waiters may already have read.
func (r *pinReady) settle(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
	})
}

// wait blocks until the registration settles, then returns its error (nil when
// it succeeded). A settled pin answers immediately even from a cancelled
// context — there is nothing left to wait for, so the context has nothing to
// say about it.
func (r *pinReady) wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.err
	default:
	}
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
