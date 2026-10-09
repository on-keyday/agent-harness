package server

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

func frameBytes(t frame.FrameType, payload []byte) []byte {
	h := frame.FrameHeader{Type: t, Len: uint32(len(payload))}
	return append(h.MustAppend(nil), payload...)
}

// scanResult collects what a scan reported, per frame type.
type scanResult struct {
	payload map[frame.FrameType][]byte
	empties []frame.FrameType
}

func scanAll(chunks [][]byte) scanResult {
	r := scanResult{payload: map[frame.FrameType][]byte{}}
	var s frameScanner
	for _, c := range chunks {
		s.scan(c,
			func(t frame.FrameType, data []byte) { r.payload[t] = append(r.payload[t], data...) },
			func(t frame.FrameType) { r.empties = append(r.empties, t) })
	}
	return r
}

func testFrameStream() []byte {
	var b []byte
	b = append(b, frameBytes(frame.FrameType_Stdout, []byte("hello"))...)
	b = append(b, frameBytes(frame.FrameType_Stderr, []byte("err"))...)
	b = append(b, frameBytes(frame.FrameType_Control, []byte("xyz"))...)
	b = append(b, frameBytes(frame.FrameType_Synth, []byte("S"))...)
	b = append(b, frameBytes(frame.FrameType(9), []byte("zz"))...)
	b = append(b, frameBytes(frame.FrameType_Stdout, nil)...) // stdout closed
	b = append(b, frameBytes(frame.FrameType_Stdout, bytes.Repeat([]byte("q"), 70000))...)
	return b
}

func checkScan(t *testing.T, r scanResult, label string) {
	t.Helper()
	if got := string(r.payload[frame.FrameType_Stdout]); got != "hello"+string(bytes.Repeat([]byte("q"), 70000)) {
		t.Fatalf("%s: stdout payload len %d", label, len(got))
	}
	if string(r.payload[frame.FrameType_Stderr]) != "err" {
		t.Fatalf("%s: stderr %q", label, r.payload[frame.FrameType_Stderr])
	}
	if string(r.payload[frame.FrameType_Synth]) != "S" || string(r.payload[frame.FrameType_Control]) != "xyz" {
		t.Fatalf("%s: synth/control payload not reported by type", label)
	}
	if len(r.empties) != 1 || r.empties[0] != frame.FrameType_Stdout {
		t.Fatalf("%s: empties %v", label, r.empties)
	}
}

// The relay's chunking has nothing to do with frame boundaries. Split the same
// stream at EVERY position into two chunks, and also one byte per chunk: the
// scanner must report the same payloads every time.
func TestFrameScannerIgnoresChunkBoundaries(t *testing.T) {
	stream := testFrameStream()
	checkScan(t, scanAll([][]byte{stream}), "whole")
	for i := 1; i < 64; i++ { // every split inside the first frames
		checkScan(t, scanAll([][]byte{stream[:i], stream[i:]}), "split")
	}
	ones := make([][]byte, len(stream))
	for i := range stream {
		ones[i] = stream[i : i+1]
	}
	checkScan(t, scanAll(ones), "one byte per chunk")

	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 50; round++ {
		var chunks [][]byte
		for rest := stream; len(rest) > 0; {
			n := 1 + rng.Intn(9000)
			if n > len(rest) {
				n = len(rest)
			}
			chunks = append(chunks, rest[:n])
			rest = rest[n:]
		}
		checkScan(t, scanAll(chunks), "random")
	}
}

func TestExecTapChannelOf(t *testing.T) {
	for typ, want := range map[frame.FrameType]protocol.ExecTapChannel{
		frame.FrameType_Stdin: protocol.ExecTapChannel_Stdin, frame.FrameType_Stdout: protocol.ExecTapChannel_Stdout,
		frame.FrameType_Synth: protocol.ExecTapChannel_Stdout, frame.FrameType_Stderr: protocol.ExecTapChannel_Stderr,
	} {
		ch, ok := execTapChannelOf(typ)
		if !ok || ch != want {
			t.Fatalf("%v -> %v %v, want %v", typ, ch, ok, want)
		}
	}
	for _, typ := range []frame.FrameType{frame.FrameType_Control, frame.FrameType(9)} {
		if _, ok := execTapChannelOf(typ); ok {
			t.Fatalf("%v must map to no channel", typ)
		}
	}
}

// frameHeaderSize is a literal in session_mux.go; the scanner decodes headers
// of exactly that many bytes, so it must equal what the schema encodes.
func TestFrameHeaderSizeMatchesTheSchema(t *testing.T) {
	if n := len((&frame.FrameHeader{}).MustAppend(nil)); n != frameHeaderSize {
		t.Fatalf("FrameHeader encodes to %d bytes, frameHeaderSize = %d", n, frameHeaderSize)
	}
}
