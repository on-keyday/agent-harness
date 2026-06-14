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

	// appendCount is the total number of frames ever Appended (monotonic,
	// never decremented by eviction). It gives each frame a stable identity:
	// the frame at frames[i] has append-index (appendCount-len(frames))+i.
	// SnapshotFrom uses it to replay only a suffix of history even as the
	// front of the ring is evicted.
	appendCount int
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
	r.appendCount++
	for r.bytes > r.cap && len(r.frames) > 1 {
		r.bytes -= len(r.frames[0])
		r.frames = r.frames[1:]
	}
}

// AppendCount returns the total number of frames ever Appended. The most
// recently appended frame has append-index AppendCount()-1; callers use this
// to record a stable replay mark (see SnapshotFrom).
func (r *RingBuffer) AppendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendCount
}

// SnapshotFrom returns the stored frames whose append-index is >= mark,
// concatenated oldest first. A mark <= the oldest surviving frame's index
// (i.e. the mark frame was already evicted, or mark is 0) yields the full
// snapshot; a mark beyond the newest frame yields nil. This lets a reattach
// replay only the tail of history — e.g. everything since a full-screen app
// left the alternate screen — without mutating the ring or risking mid-frame
// truncation (the unit of trimming is always a whole frame).
func (r *RingBuffer) SnapshotFrom(mark int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bytes == 0 {
		return nil
	}
	firstIdx := r.appendCount - len(r.frames)
	start := mark - firstIdx
	if start < 0 {
		start = 0
	}
	if start >= len(r.frames) {
		return nil
	}
	n := 0
	for _, f := range r.frames[start:] {
		n += len(f)
	}
	out := make([]byte, 0, n)
	for _, f := range r.frames[start:] {
		out = append(out, f...)
	}
	return out
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
