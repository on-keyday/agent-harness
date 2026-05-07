package server

import "sync"

// RingBuffer is a fixed-size raw-byte ring. Appends past capacity overwrite
// the oldest bytes. Safe for concurrent use.
type RingBuffer struct {
	mu       sync.Mutex
	buf      []byte
	cap      int
	writeIdx int  // next write position
	full     bool // true once buf has wrapped at least once
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{buf: make([]byte, capacity), cap: capacity}
}

func (r *RingBuffer) Append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.cap {
		// only the last `cap` bytes survive
		copy(r.buf, p[len(p)-r.cap:])
		r.writeIdx = 0
		r.full = true
		return
	}
	tail := r.cap - r.writeIdx
	if len(p) <= tail {
		copy(r.buf[r.writeIdx:], p)
	} else {
		copy(r.buf[r.writeIdx:], p[:tail])
		copy(r.buf[0:], p[tail:])
		r.full = true
	}
	r.writeIdx = (r.writeIdx + len(p)) % r.cap
	if r.writeIdx == 0 && len(p) > 0 {
		r.full = true
	}
}

// Snapshot returns the buffer content in oldest-to-newest order.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.writeIdx)
		copy(out, r.buf[:r.writeIdx])
		return out
	}
	out := make([]byte, r.cap)
	copy(out, r.buf[r.writeIdx:])
	copy(out[r.cap-r.writeIdx:], r.buf[:r.writeIdx])
	return out
}

func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return r.cap
	}
	return r.writeIdx
}
