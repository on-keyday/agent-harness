//go:build !js

package sshgw

import "sync"

// execTTY is what a pty-req asked for, carried to the exec that follows it.
type execTTY struct {
	term string
	dims ptyDims
}

// execResizer hands window-change to a PTY exec whose stream may not be open
// yet. Until set, the last size is kept; set applies it, so whichever arrives
// first, the exec ends up at the size the client last reported.
type execResizer struct {
	mu   sync.Mutex
	fn   func(rows, cols, widthPx, heightPx uint16) error
	last *ptyDims
}

func (r *execResizer) apply(d ptyDims) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fn == nil {
		r.last = &d
		return
	}
	// A resize the stream cannot take means the stream is gone, and the exec's
	// own end reports that; there is nothing more to say from here.
	_ = r.fn(d.Rows, d.Cols, d.WidthPx, d.HeightPx)
}

func (r *execResizer) set(fn func(rows, cols, widthPx, heightPx uint16) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn = fn
	if r.last != nil {
		_ = fn(r.last.Rows, r.last.Cols, r.last.WidthPx, r.last.HeightPx)
		r.last = nil
	}
}
