package server

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/exec/frame"
)

func TestCapReplayTail(t *testing.T) {
	var data []byte
	var frames [][]byte
	for i := 0; i < 10; i++ {
		f := makeWireFrame(byte(frame.FrameType_Stdout), bytes.Repeat([]byte{byte('a' + i)}, 100))
		frames = append(frames, f)
		data = append(data, f...)
	}

	if got := capReplayTail(data, 0); !bytes.Equal(got, data) {
		t.Fatal("limit 0 must return the full replay")
	}
	if got := capReplayTail(data, 1<<20); !bytes.Equal(got, data) {
		t.Fatal("data smaller than limit must return the full replay")
	}

	got := capReplayTail(data, 250)
	// Must be a frame-aligned suffix (equal to the concatenation of the last K frames).
	aligned := false
	acc := []byte{}
	for i := len(frames) - 1; i >= 0; i-- {
		acc = append(append([]byte{}, frames[i]...), acc...)
		if bytes.Equal(got, acc) {
			aligned = true
			break
		}
	}
	if !aligned {
		t.Fatalf("capped replay is not frame-aligned (%d bytes)", len(got))
	}
	if len(got) == 0 || len(got) > len(data) {
		t.Fatalf("capped replay length %d out of range", len(got))
	}
	// Each frame is 105 bytes; limit 250 => keep ~2-3 frames, not the whole 1050.
	if len(got) > 250+105 {
		t.Fatalf("capped replay %d exceeds limit 250 by more than one frame", len(got))
	}

	// A single frame larger than the limit is kept whole (never empty/split).
	big := makeWireFrame(byte(frame.FrameType_Stdout), bytes.Repeat([]byte{'z'}, 500))
	if got := capReplayTail(big, 100); !bytes.Equal(got, big) {
		t.Fatalf("a single frame > limit must be kept whole, got %d bytes", len(got))
	}
}

// An observer attach with a replay limit must receive far less than a big ring;
// an uncapped one (limit 0) gets the whole ring.
func TestSessionMux_Observer_ReplayCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	// Fill the ring with ~300 KiB across many frames.
	total := 0
	for total < 300*1024 {
		f := makeWireFrame(byte(frame.FrameType_Stdout), bytes.Repeat([]byte{'x'}, 1000))
		runner.QueueRead(f)
		total += len(f)
	}
	waitFor(t, func() bool { return mux.RingBufferLen() >= 300*1024 })

	const limit = 128 * 1024
	cw := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, cw, limit); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	waitFor(t, func() bool { return len(cw.Written()) > 0 })
	time.Sleep(150 * time.Millisecond) // let the replay burst settle
	if n := len(cw.Written()); n > limit+16*1024 {
		t.Fatalf("capped observer replay %d exceeds limit %d + margin (ring ~300 KiB)", n, limit)
	}

	full := newFakeStream(t)
	if err := mux.AttachViewer(ctx, full, 0); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	waitFor(t, func() bool { return len(full.Written()) >= 300*1024 })
}
