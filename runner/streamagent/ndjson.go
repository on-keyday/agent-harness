package streamagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// maxLine bounds one NDJSON line. The probe found hook_response events
// carrying a whole injected-context blob, so "a line is small" is not an
// assumption this seam gets to make; it is a limit it states.
const maxLine = 8 << 20 // 8 MiB

// Writer serialises Msgs onto a stream, one line each. Every write is under
// the mutex: the adapter writes events from the agent's stdout pump and exit
// from another goroutine, and two interleaved halves of a JSON line is a
// corruption that reads as a protocol error much later.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) Write(m Msg) error {
	m.V = ProtocolVersion
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", m.Kind, err)
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(b)
	return err
}

// Event, Request and the rest are helpers so callers name the kind once
// instead of pairing a Kind with a payload field by hand — a pair that can be
// got wrong silently (kind=event with only Request set marshals fine).
func (w *Writer) Event(e Event) error       { return w.Write(Msg{Kind: KindEvent, Event: &e}) }
func (w *Writer) Request(r Request) error   { return w.Write(Msg{Kind: KindRequest, Request: &r}) }
func (w *Writer) Response(r Response) error { return w.Write(Msg{Kind: KindResponse, Response: &r}) }
func (w *Writer) User(t UserTurn) error     { return w.Write(Msg{Kind: KindUser, User: &t}) }
func (w *Writer) Hello(h Hello) error       { return w.Write(Msg{Kind: KindHello, Hello: &h}) }
func (w *Writer) Exit(e Exit) error         { return w.Write(Msg{Kind: KindExit, Exit: &e}) }

// DecodeMsg parses one NDJSON line. It exists so a consumer that already has
// line boundaries -- the runner's Auditor tap, which is handed raw payload
// chunks rather than a stream -- can decode without standing up a Reader.
func DecodeMsg(line []byte) (Msg, error) {
	var m Msg
	if err := json.Unmarshal(line, &m); err != nil {
		return Msg{}, fmt.Errorf("malformed line: %w", err)
	}
	if m.V != 0 && m.V != ProtocolVersion {
		return Msg{}, fmt.Errorf("protocol version %d, this build speaks %d", m.V, ProtocolVersion)
	}
	return m, nil
}

// Reader decodes Msgs from a stream.
type Reader struct{ sc *bufio.Scanner }

func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	return &Reader{sc: sc}
}

// Next returns the next message. io.EOF marks a clean end of stream. A line
// that does not parse is an error the CALLER decides about: on the adapter's
// own input a malformed line is a protocol violation worth failing on, while
// the agent's output gets the opposite treatment (see decodeAgentLine), and
// this type should not make that choice for both.
func (r *Reader) Next() (Msg, error) {
	for {
		if !r.sc.Scan() {
			if err := r.sc.Err(); err != nil {
				return Msg{}, err
			}
			return Msg{}, io.EOF
		}
		line := r.sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		return DecodeMsg(line)
	}
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}
