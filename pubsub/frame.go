package pubsub

import (
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/pubsub/protocol"
)

// MaxFrameLen is the length at which a frame is refused rather than awaited.
// It is a cap on a single message, far above any event or log chunk the broker
// sends; its job is to recognise a stream that is not framed at all — a server
// older than this client sends raw events, whose first four bytes read as a
// length of at least this much whenever the first byte is non-zero.
const MaxFrameLen = 16 << 20

// FrameWriter is the send side WriteFrame needs; trsf streams satisfy it.
type FrameWriter interface {
	AppendData(eof bool, data ...[]byte) error
}

// WriteFrame writes msg to w as one PubSubFrame, in ONE AppendData call so two
// publishers writing the same subscriber cannot interleave.
//
// The frame is a fresh buffer, never msg itself: AppendData takes ownership of
// what it is handed (objtrsf trsf/send_stream.go: "data must be copied before
// calling AppendData") — the stream keeps it until ACKed and may append onto
// it — and the broker hands one msg to every subscriber and to its taps.
func WriteFrame(w FrameWriter, msg []byte) error {
	if len(msg) >= MaxFrameLen {
		return fmt.Errorf("pubsub: message of %d bytes is at or over the %d-byte frame cap", len(msg), MaxFrameLen)
	}
	f := protocol.PubSubFrame{}
	if !f.SetData(msg) {
		return fmt.Errorf("pubsub: message of %d bytes does not fit a frame", len(msg))
	}
	return w.AppendData(false, f.MustAppend(nil))
}

// StreamReader is the receive side FrameReader needs; trsf streams satisfy it.
type StreamReader interface {
	ReadDirect(maxN uint64) ([]byte, bool, error)
}

// FrameTooLargeError is a frame length at or over MaxFrameLen.
type FrameTooLargeError struct{ Len uint32 }

func (e *FrameTooLargeError) Error() string {
	return fmt.Sprintf("pubsub: frame of %d bytes exceeds the 16 MiB cap — is the server older than this client?", e.Len)
}

// FrameReader returns a subscriber stream's messages one at a time. A trsf read
// returns whatever contiguous bytes have arrived — part of a frame, or several
// — so the reader keeps what it has not yet returned.
type FrameReader struct {
	st  StreamReader
	buf []byte
	eof bool
	err error
}

func NewFrameReader(st StreamReader) *FrameReader { return &FrameReader{st: st} }

// Next returns the next message. The slice is the caller's: it does not alias
// the reader's buffer. Errors: io.EOF (the stream ended between frames),
// io.ErrUnexpectedEOF (it ended inside one), *FrameTooLargeError, or the
// stream's own read error. After any error, Next returns it again without
// reading.
func (r *FrameReader) Next() ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	for {
		var h protocol.PubSubFrameHeader
		if rest, err := h.Decode(r.buf); err == nil {
			if h.DataLen >= MaxFrameLen {
				r.err = &FrameTooLargeError{Len: h.DataLen}
				return nil, r.err
			}
			if uint64(len(rest)) >= uint64(h.DataLen) {
				var f protocol.PubSubFrame
				tail, err := f.Decode(r.buf)
				if err != nil {
					// The header decoded and the payload is all here, so the
					// frame cannot be short; this is a generated-code bug.
					r.err = fmt.Errorf("pubsub: decode frame: %w", err)
					return nil, r.err
				}
				out := append([]byte(nil), f.Data...)
				r.buf = append(r.buf[:0], tail...)
				return out, nil
			}
		}
		if r.eof {
			if len(r.buf) == 0 {
				r.err = io.EOF
			} else {
				r.err = io.ErrUnexpectedEOF
			}
			return nil, r.err
		}
		data, eof, err := r.st.ReadDirect(64 * 1024)
		if err != nil {
			r.err = err
			return nil, r.err
		}
		r.buf = append(r.buf, data...)
		r.eof = eof
	}
}
