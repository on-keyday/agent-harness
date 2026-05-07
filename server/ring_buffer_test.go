package server

import (
	"bytes"
	"testing"
)

func TestRingBuffer_AppendUnderCapacity(t *testing.T) {
	rb := NewRingBuffer(16)
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

func TestRingBuffer_WrapAround(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Append([]byte("abcdefghij")) // 10 bytes into 8-byte buffer
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("cdefghij")) {
		t.Fatalf("got %q want %q", got, "cdefghij")
	}
	if rb.Len() != 8 {
		t.Fatalf("Len=%d want 8", rb.Len())
	}
}

func TestRingBuffer_AppendLargerThanCap(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Append([]byte("0123456789"))
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("6789")) {
		t.Fatalf("got %q want %q", got, "6789")
	}
}

func TestRingBuffer_EmptySnapshot(t *testing.T) {
	rb := NewRingBuffer(8)
	if len(rb.Snapshot()) != 0 {
		t.Fatalf("empty buffer Snapshot should be empty")
	}
}

func TestRingBuffer_ExactBoundary(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Append([]byte("ab"))
	rb.Append([]byte("cd"))
	// writeIdx returned to 0 after landing exactly at capacity boundary.
	// Snapshot must return all 4 bytes in order.
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("abcd")) {
		t.Fatalf("got %q want %q", got, "abcd")
	}
	if rb.Len() != 4 {
		t.Fatalf("Len=%d want 4", rb.Len())
	}
	// Next append should wrap.
	rb.Append([]byte("X"))
	got = rb.Snapshot()
	if !bytes.Equal(got, []byte("bcdX")) {
		t.Fatalf("after wrap got %q want %q", got, "bcdX")
	}
}
