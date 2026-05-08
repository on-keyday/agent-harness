package server

import "sync"

// RingBuffer holds up to a soft byte budget of wire-encoded frames in
// append order. Drops always happen on a *frame boundary*: when an Append
// pushes the total past cap, the oldest whole frames are evicted until the
// new total fits, or until only the just-pushed frame remains. A single
// frame larger than cap is stored alone (cap is exceeded for that one
// entry) — the alternative would be silently dropping data the caller
// already considered atomic.
//
// Each Append() must receive exactly one complete wire-encoded frame
// (header + payload). Truncation at arbitrary byte offsets would corrupt
// the consumer's frame parser when the ring later wraps mid-frame; the
// API is shaped to make that mistake impossible at the type level.
type RingBuffer struct {
	mu     sync.Mutex
	frames [][]byte
	cap    int // soft byte budget
	bytes  int // sum of len(frames[i])
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{cap: capacity}
}

// Append stores p as one ring entry. The caller MUST pass exactly one
// complete wire-encoded frame; passing partial or multi-frame slices will
// corrupt replay. The bytes are copied — caller may reuse its buffer.
func (r *RingBuffer) Append(p []byte) {
	if len(p) == 0 {
		return
	}
	frame := append([]byte(nil), p...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, frame)
	r.bytes += len(frame)
	for r.bytes > r.cap && len(r.frames) > 1 {
		r.bytes -= len(r.frames[0])
		r.frames = r.frames[1:]
	}
}

// Snapshot returns all stored frames concatenated, oldest first.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bytes == 0 {
		return nil
	}
	out := make([]byte, 0, r.bytes)
	for _, f := range r.frames {
		out = append(out, f...)
	}
	return out
}

// Len returns the current total byte size of all stored frames.
func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}
