package pubsub

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/pubsub/protocol"
)

// chunkStream serves its chunks one per ReadDirect, then EOF.
type chunkStream struct {
	chunks [][]byte
	reads  int
}

func (s *chunkStream) ReadDirect(uint64) ([]byte, bool, error) {
	s.reads++
	if len(s.chunks) == 0 {
		return nil, true, nil
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, len(s.chunks) == 0, nil
}

// frameBytes encodes msgs the way WriteFrame puts them on the wire.
func frameBytes(t *testing.T, msgs ...[]byte) []byte {
	t.Helper()
	var w appendRecorder
	for _, m := range msgs {
		if err := WriteFrame(&w, m); err != nil {
			t.Fatal(err)
		}
	}
	return bytes.Join(w.flat(), nil)
}

type appendRecorder struct {
	mu    sync.Mutex
	calls [][][]byte
}

func (a *appendRecorder) AppendData(_ bool, data ...[]byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([][]byte, len(data))
	for i, d := range data {
		cp[i] = append([]byte(nil), d...)
	}
	a.calls = append(a.calls, cp)
	return nil
}

func (a *appendRecorder) flat() [][]byte {
	var out [][]byte
	for _, c := range a.calls {
		out = append(out, c...)
	}
	return out
}

func readAll(t *testing.T, r *FrameReader) ([][]byte, error) {
	t.Helper()
	var got [][]byte
	for {
		p, err := r.Next()
		if err != nil {
			return got, err
		}
		got = append(got, p)
	}
}

var testMsgs = [][]byte{[]byte("alpha"), {}, bytes.Repeat([]byte("x"), 5000), []byte("omega")}

func checkMsgs(t *testing.T, got [][]byte, err error) {
	t.Helper()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(got) != len(testMsgs) {
		t.Fatalf("got %d frames, want %d", len(got), len(testMsgs))
	}
	for i := range testMsgs {
		if !bytes.Equal(got[i], testMsgs[i]) {
			t.Fatalf("frame %d = %d bytes, want %d", i, len(got[i]), len(testMsgs[i]))
		}
	}
}

func TestFrameReader_AllAtOnce(t *testing.T) {
	wire := frameBytes(t, testMsgs...)
	got, err := readAll(t, NewFrameReader(&chunkStream{chunks: [][]byte{wire}}))
	checkMsgs(t, got, err)
}

func TestFrameReader_OneByteAtATime(t *testing.T) {
	wire := frameBytes(t, testMsgs...)
	var chunks [][]byte
	for i := range wire {
		chunks = append(chunks, wire[i:i+1])
	}
	got, err := readAll(t, NewFrameReader(&chunkStream{chunks: chunks}))
	checkMsgs(t, got, err)
}

func TestFrameReader_SplitInsideLengthAndPayload(t *testing.T) {
	wire := frameBytes(t, testMsgs...)
	// 2: inside the first length. 7: inside "alpha". 11: inside the empty
	// frame's length. 2000: inside the 5000-byte payload.
	cuts := []int{2, 7, 11, 2000, len(wire)}
	var chunks [][]byte
	prev := 0
	for _, c := range cuts {
		chunks = append(chunks, wire[prev:c])
		prev = c
	}
	got, err := readAll(t, NewFrameReader(&chunkStream{chunks: chunks}))
	checkMsgs(t, got, err)
}

func TestFrameReader_EndsMidFrame(t *testing.T) {
	wire := frameBytes(t, []byte("whole"), []byte("partial"))
	got, err := readAll(t, NewFrameReader(&chunkStream{chunks: [][]byte{wire[:len(wire)-3]}}))
	if len(got) != 1 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %d frames, err %v; want 1 frame then io.ErrUnexpectedEOF", len(got), err)
	}
}

func TestFrameReader_OverCap(t *testing.T) {
	hdr := (&protocol.PubSubFrameHeader{DataLen: MaxFrameLen}).MustAppend(nil)
	_, err := NewFrameReader(&chunkStream{chunks: [][]byte{hdr}}).Next()
	var tl *FrameTooLargeError
	if !errors.As(err, &tl) || tl.Len != MaxFrameLen {
		t.Fatalf("err = %v, want *FrameTooLargeError{Len: %d}", err, MaxFrameLen)
	}
	want := "pubsub: frame of 16777216 bytes exceeds the 16 MiB cap — is the server older than this client?"
	if err.Error() != want {
		t.Fatalf("text = %q, want %q", err.Error(), want)
	}
}

// One byte under the cap is a frame, not an error: the reader waits for it.
func TestFrameReader_JustUnderCapIsNotAnError(t *testing.T) {
	hdr := (&protocol.PubSubFrameHeader{DataLen: MaxFrameLen - 1}).MustAppend(nil)
	_, err := NewFrameReader(&chunkStream{chunks: [][]byte{hdr}}).Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF (a frame that never completed)", err)
	}
}

func TestFrameReader_EOFIsSticky(t *testing.T) {
	s := &chunkStream{chunks: [][]byte{frameBytes(t, []byte("a"))}}
	r := NewFrameReader(s)
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("call %d: err = %v, want io.EOF", i, err)
		}
	}
	if s.reads > 2 {
		t.Fatalf("ReadDirect called %d times after EOF; a finished reader must not read", s.reads)
	}
}

func TestFrameReader_ReadErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	r := NewFrameReader(errStream{boom})
	if _, err := r.Next(); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

type errStream struct{ err error }

func (e errStream) ReadDirect(uint64) ([]byte, bool, error) { return nil, false, e.err }

func TestFrameReader_PayloadDoesNotAlias(t *testing.T) {
	wire := frameBytes(t, []byte("first"), []byte("second"))
	r := NewFrameReader(&chunkStream{chunks: [][]byte{wire}})
	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if string(first) != "first" {
		t.Fatalf("first payload became %q after the next call", first)
	}
}

func TestWriteFrame_OneAppendCall(t *testing.T) {
	var w appendRecorder
	if err := WriteFrame(&w, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 {
		t.Fatalf("WriteFrame made %d AppendData calls, want 1 (header and payload must not interleave with another writer)", len(w.calls))
	}
	var f protocol.PubSubFrame
	if _, err := f.Decode(bytes.Join(w.calls[0], nil)); err != nil || string(f.Data) != "payload" {
		t.Fatalf("decoded %q, %v; want \"payload\"", f.Data, err)
	}
}

func TestWriteFrame_OverCapIsRefused(t *testing.T) {
	var w appendRecorder
	err := WriteFrame(&w, make([]byte, MaxFrameLen))
	if err == nil || !strings.Contains(err.Error(), "cap") || len(w.calls) != 0 {
		t.Fatalf("err = %v, calls = %d; want a cap error and nothing written", err, len(w.calls))
	}
}

// keepStream keeps the slices it is handed, without copying, as trsf does.
type keepStream struct{ data [][]byte }

func (k *keepStream) AppendData(_ bool, data ...[]byte) error {
	k.data = append(k.data, data...)
	return nil
}

// AppendData takes ownership of what it is handed (objtrsf
// trsf/send_stream.go: "data must be copied before calling AppendData"): the
// stream keeps the slice until it is ACKed and may append onto it. The broker
// hands one msg to every subscriber, so each stream must get bytes of its own.
func TestWriteFrame_StreamOwnsWhatItIsHanded(t *testing.T) {
	msg := []byte("hello")
	var a, b keepStream
	if err := WriteFrame(&a, msg); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, msg); err != nil {
		t.Fatal(err)
	}
	copy(msg, "XXXXX") // the publisher reuses its buffer
	for _, d := range a.data {
		d[len(d)-1] = '!' // stream a writes into what it owns
	}
	var f protocol.PubSubFrame
	if _, err := f.Decode(bytes.Join(b.data, nil)); err != nil || string(f.Data) != "hello" {
		t.Fatalf("stream b sends %q (%v), want \"hello\": its bytes are shared with the publisher or with stream a", f.Data, err)
	}
}
