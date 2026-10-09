package server

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// frameScanner follows objtrsf exec frame boundaries (type, length, payload)
// across arbitrary chunking, from the stream's first byte. It never copies and
// never fails: any type byte and any length are legal, and the relay forwards
// the bytes unchanged whatever the scanner concluded. See the exec tap spec,
// § Why the server must parse frames.
type frameScanner struct {
	hdr       []byte // header bytes collected so far (< frameHeaderSize, session_mux.go)
	typ       frame.FrameType
	remaining uint32 // payload bytes left in the current frame
	inPayload bool
}

// scan walks one chunk. onPayload receives each payload run (aliasing chunk —
// the callee must copy to retain it); onEmpty receives each zero-length frame.
func (s *frameScanner) scan(chunk []byte, onPayload func(frame.FrameType, []byte), onEmpty func(frame.FrameType)) {
	for len(chunk) > 0 {
		if s.inPayload {
			n := uint32(len(chunk))
			if n > s.remaining {
				n = s.remaining
			}
			onPayload(s.typ, chunk[:n])
			chunk = chunk[n:]
			s.remaining -= n
			if s.remaining == 0 {
				s.inPayload = false
			}
			continue
		}
		need := frameHeaderSize - len(s.hdr)
		if need > len(chunk) {
			s.hdr = append(s.hdr, chunk...)
			return
		}
		s.hdr = append(s.hdr, chunk[:need]...)
		chunk = chunk[need:]
		var h frame.FrameHeader
		// DecodeExact cannot fail on exactly frameHeaderSize bytes of a
		// fixed-layout header; TestFrameHeaderSizeMatchesTheSchema pins the size.
		_ = h.DecodeExact(s.hdr)
		s.hdr = s.hdr[:0]
		s.typ = h.Type
		if h.Len == 0 {
			onEmpty(h.Type)
			continue
		}
		s.remaining = h.Len
		s.inPayload = true
	}
}

// execTapChannelOf maps a frame type to the channel a tap reports it on. Synth
// is stdout because the client's demux writes it there
// (objtrsf/exec/exec_stream.go). Control and unknown types are no channel.
func execTapChannelOf(t frame.FrameType) (protocol.ExecTapChannel, bool) {
	switch t {
	case frame.FrameType_Stdin:
		return protocol.ExecTapChannel_Stdin, true
	case frame.FrameType_Stdout, frame.FrameType_Synth:
		return protocol.ExecTapChannel_Stdout, true
	case frame.FrameType_Stderr:
		return protocol.ExecTapChannel_Stderr, true
	}
	return 0, false
}
