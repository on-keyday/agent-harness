package protocol_test

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A detached open carries its initial PTY size in the request, because there is
// no client stream to carry the window-size frame. The size block exists only
// under detached == 1, so an attached open puts no size bytes on the wire.
func TestOpenInteractiveRequestDetachedRoundTrip(t *testing.T) {
	build := func(detached bool) protocol.OpenInteractiveRequest {
		r := protocol.OpenInteractiveRequest{}
		r.SetRepoPath([]byte("/r"))
		r.SetResumeExact(true)
		r.SetDetached(detached)
		if detached {
			r.SetInitialRows(40)
			r.SetInitialCols(150)
		}
		return r
	}

	det := build(true)
	wire := det.MustAppend(nil)
	var got protocol.OpenInteractiveRequest
	if err := got.DecodeExact(wire); err != nil {
		t.Fatalf("DecodeExact: %v", err)
	}
	if !got.Detached() || !got.ResumeExact() {
		t.Fatalf("flags lost: detached=%v resume_exact=%v", got.Detached(), got.ResumeExact())
	}
	if r, c := got.InitialRows(), got.InitialCols(); r == nil || c == nil || *r != 40 || *c != 150 {
		t.Fatalf("initial size = %v x %v, want 40 x 150", r, c)
	}

	att := build(false)
	if d := len(wire) - len(att.MustAppend(nil)); d != 4 {
		t.Fatalf("detached encoding is %d bytes longer than attached, want 4 (the size block)", d)
	}
}

// Stream id 0 is a real id: the server's trsf issues bidirectional ids from 0.
// So "no stream" is the attached bit, and an attached open on stream 0 must
// read back as attached with stream 0.
func TestOpenInteractiveResponseAttachedStreamZero(t *testing.T) {
	r := protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_Ok}
	r.SetAttached(true)
	if !r.SetStreamId(0) {
		t.Fatal("SetStreamId refused with attached set")
	}
	wire := r.MustAppend(nil)
	var got protocol.OpenInteractiveResponse
	if err := got.DecodeExact(wire); err != nil {
		t.Fatalf("DecodeExact: %v", err)
	}
	if !got.Attached() {
		t.Fatal("attached lost")
	}
	if id := got.StreamId(); id == nil || *id != 0 {
		t.Fatalf("stream id = %v, want 0", id)
	}

	det := protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_Ok}
	if d := len(wire) - len(det.MustAppend(nil)); d != 8 {
		t.Fatalf("attached encoding is %d bytes longer than detached, want 8 (the stream id)", d)
	}
}
