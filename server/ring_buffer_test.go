package server

import (
	"bytes"
	"testing"
)

// TestRingBuffer_AppendUnderCapacity verifies that frames within the byte
// budget are stored intact and Snapshot returns them concatenated in
// append order.
func TestRingBuffer_AppendUnderCapacity(t *testing.T) {
	rb := NewRingBuffer(64)
	rb.Append([]byte("hello"))
	rb.Append([]byte(" world"))
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("got %q want %q", got, "hello world")
	}
	if rb.Len() != 11 {
		t.Fatalf("Len=%d want 11", rb.Len())
	}
}

// TestRingBuffer_DropsWholeOldestFrame verifies that an Append that pushes
// past cap evicts the OLDEST frame entirely — never a partial frame.
func TestRingBuffer_DropsWholeOldestFrame(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Append([]byte("frame1")) // 6 bytes
	rb.Append([]byte("f2"))     // 2 bytes (total 8 — at cap)
	rb.Append([]byte("f3"))     // 2 bytes (10, over) → evict frame1 (6) → total 4
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("f2f3")) {
		t.Fatalf("got %q want %q", got, "f2f3")
	}
	if rb.Len() != 4 {
		t.Fatalf("Len=%d want 4", rb.Len())
	}
}

// TestRingBuffer_DropsMultipleOldFrames verifies the eviction loop runs
// until total bytes fit (or only the new frame remains).
func TestRingBuffer_DropsMultipleOldFrames(t *testing.T) {
	rb := NewRingBuffer(10)
	rb.Append([]byte("aaa"))
	rb.Append([]byte("bbb"))
	rb.Append([]byte("ccc")) // 9 bytes total
	rb.Append([]byte("dddddddd")) // 8 bytes — evict aaa,bbb,ccc (9) → total 8
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("dddddddd")) {
		t.Fatalf("got %q want %q", got, "dddddddd")
	}
	if rb.Len() != 8 {
		t.Fatalf("Len=%d want 8", rb.Len())
	}
}

// TestRingBuffer_KeepsOversizedFrameAlone verifies that a single frame
// larger than cap is stored alone, exceeding cap. Losing data the caller
// considered atomic is worse than a temporary memory blowup; cap is a
// soft budget.
func TestRingBuffer_KeepsOversizedFrameAlone(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Append([]byte("0123456789"))
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("0123456789")) {
		t.Fatalf("got %q want %q", got, "0123456789")
	}
	if rb.Len() != 10 {
		t.Fatalf("Len=%d want 10", rb.Len())
	}
}

// TestRingBuffer_OversizedFrameEvictsHistory verifies that pushing an
// oversized frame after smaller ones evicts the smaller history, leaving
// only the oversized entry.
func TestRingBuffer_OversizedFrameEvictsHistory(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Append([]byte("aa"))
	rb.Append([]byte("bb"))
	rb.Append([]byte("0123456789")) // 10 bytes > cap=8 → evict aa, bb
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("0123456789")) {
		t.Fatalf("got %q want %q", got, "0123456789")
	}
}

// TestRingBuffer_EmptySnapshot verifies the zero-state Snapshot is empty.
func TestRingBuffer_EmptySnapshot(t *testing.T) {
	rb := NewRingBuffer(8)
	if len(rb.Snapshot()) != 0 {
		t.Fatalf("empty buffer Snapshot should be empty")
	}
	if rb.Len() != 0 {
		t.Fatalf("Len=%d want 0", rb.Len())
	}
}

// TestRingBuffer_AppendEmptyIsNoop verifies that an Append of zero bytes
// is a no-op (does not insert a phantom frame entry).
func TestRingBuffer_AppendEmptyIsNoop(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Append([]byte("ab"))
	rb.Append(nil)
	rb.Append([]byte{})
	rb.Append([]byte("cd"))
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("abcd")) {
		t.Fatalf("got %q want %q", got, "abcd")
	}
	if rb.Len() != 4 {
		t.Fatalf("Len=%d want 4", rb.Len())
	}
}

// TestRingBuffer_AppendDoesNotAliasCallerBuffer verifies that callers can
// safely reuse their input buffer after Append (the ring stores a copy).
func TestRingBuffer_AppendDoesNotAliasCallerBuffer(t *testing.T) {
	rb := NewRingBuffer(16)
	buf := []byte("hello")
	rb.Append(buf)
	for i := range buf {
		buf[i] = 'X'
	}
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("ring aliased caller buffer: got %q want %q", got, "hello")
	}
}
