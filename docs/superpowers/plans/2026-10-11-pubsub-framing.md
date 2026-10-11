# Pubsub Framing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The broker length-prefixes every message it writes to a subscriber, and every subscriber reads frames, so an event split across reads (a long-argv `ExecStatusEvent`) or coalesced with another is delivered whole.

**Architecture:** A `PubSubFrame` format in `pubsub/protocol/message.bgn`; `pubsub.WriteFrame` (writer) and `pubsub.FrameReader` (reader) in a new `pubsub/frame.go`. The two producers (`PublishFiltered`, the notifications replay via `OnSubscribe`) write through `WriteFrame`; the six readers in `tui/` and `cli/` read through `FrameReader`, and the per-format `drain*` helpers are deleted. The publishing direction (runner → broker) is untouched.

**Tech Stack:** Go, brgen `.bgn` (`make protoregen ARGS=pubsub/protocol/message.bgn`), objtrsf trsf streams, bubbletea (TUI).

**Spec:** `docs/superpowers/specs/2026-10-11-pubsub-framing-design.md`

## Global Constraints

- Only broker → subscriber is framed (D3). `peer.Conn.Publish`, `runner/`, and the broker's read of a publisher's stream (`pubsub.go` `Subscribe`'s goroutine) do not change.
- Frame payloads are decoded with `Decode`, never `DecodeExact` (D5).
- A payload that does not decode: `slog.Warn` naming the topic and error, then the next frame (D6).
- Cap: a frame length `>= 16 MiB` (`16 << 20`) is an error whose text is exactly `pubsub: frame of N bytes exceeds the 16 MiB cap — is the server older than this client?` with N the decimal length (D7, D9).
- `PubSub.OnSubscribe` receives `send func(msg []byte) error`, not the stream (D10). Length field `u32` (D11).
- The repo is public: no LAN IPs, hostnames, or private paths in committed files.
- `cli/` must still build under `GOOS=js GOARCH=wasm` (`make wasm-check`).
- Verify with make targets (`make test`, `make vet`, `make wasm-check`), plus `go test -tags integration ./integration/ -run <name>` for integration tests.

## Review Focus

1. A frame split at every possible byte offset — including inside the 4-byte length — must still be returned whole; covered by Task 1's one-byte-at-a-time test, but a reviewer should check `Next` never decodes a header from fewer bytes than it has.
2. `FrameReader.Next` after an error or `io.EOF` must not loop forever or re-read a dead stream; a TUI `resubscribeLoop` depends on the pump returning.
3. Two concurrent `PublishFiltered` calls to one subscriber must not interleave a header of one with the payload of the other — `WriteFrame` must pass header and payload in ONE `AppendData` call (trsf serializes per call via `writeFence`).
4. The task log over frames: `harness-cli logs -f` must still end when the stream ends (EOF → return nil), and the TUI log pane must get each payload exactly once.
5. `FrameReader` returns a payload that must not alias its internal buffer, since the buffer is compacted on the next call; a consumer that keeps the slice (TUI `LogChunkMsg`) would otherwise see it overwritten.

Each line's test is in the owning task (1 → Task 1 `TestFrameReader_OneByteAtATime`; 2 → Task 1 `TestFrameReader_EOFIsSticky`; 3 → Task 1 `TestWriteFrame_OneAppendCall`; 4 → Task 3 Step 9 live check; 5 → Task 1 `TestFrameReader_PayloadDoesNotAlias`).

---

### Task 1: Frame format and the pubsub writer/reader

**Files:**
- Modify: `pubsub/protocol/message.bgn` (append two formats)
- Regenerate: `pubsub/protocol/message.go`
- Create: `pubsub/frame.go`
- Test: `pubsub/frame_test.go`

**Interfaces:**
- Produces:
  - `protocol.PubSubFrameHeader{DataLen uint32}` and `protocol.PubSubFrame{DataLen uint32; Data []uint8}` (generated; `Decode(buf) (rest []byte, err error)`, `MustAppend(buf) []byte`, `SetData([]uint8) bool`)
  - `const pubsub.MaxFrameLen = 16 << 20`
  - `type pubsub.FrameWriter interface { AppendData(eof bool, data ...[]byte) error }`
  - `func pubsub.WriteFrame(w FrameWriter, msg []byte) error`
  - `type pubsub.StreamReader interface { ReadDirect(maxN uint64) ([]byte, bool, error) }`
  - `type pubsub.FrameTooLargeError struct{ Len uint32 }` (`Error() string`)
  - `func pubsub.NewFrameReader(st StreamReader) *FrameReader`
  - `func (r *FrameReader) Next() ([]byte, error)` — a payload (a fresh slice), or `io.EOF` (stream ended on a frame boundary), `io.ErrUnexpectedEOF` (ended mid-frame), `*FrameTooLargeError`, or the stream's read error. Once it has returned an error it returns the same error again without reading.

- [ ] **Step 1: Add the formats to `.bgn`**

Append to `pubsub/protocol/message.bgn`:

```
# PubSubFrame is the unit of a broker → subscriber pubsub stream: every byte the
# broker writes to a subscriber's stream is a sequence of these. The other
# direction — a subscriber publishing — is raw bytes, and each read the broker
# makes of it becomes one message.
format PubSubFrame:
    data_len :u32
    data :[data_len]u8

# PubSubFrameHeader is PubSubFrame's length alone: the writer sends it beside
# the payload so a large payload is not copied, and the reader decodes it to
# check the length against its cap before the payload has arrived.
format PubSubFrameHeader:
    data_len :u32
```

- [ ] **Step 2: Regenerate and check the symbols**

Run: `make protoregen ARGS=pubsub/protocol/message.bgn && go build ./pubsub/... && grep -n "func (p \*PubSubFrame) SetData\|^type PubSubFrameHeader struct\|^type PubSubFrame struct" pubsub/protocol/message.go`
Expected: build OK; three grep hits. If the setter's receiver name differs from `p`, adjust only the grep — the method name is what matters.

- [ ] **Step 3: Write the failing tests**

Create `pubsub/frame_test.go`:

```go
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
```

- [ ] **Step 4: Run them to see them fail**

Run: `go test ./pubsub/ -run 'TestFrameReader|TestWriteFrame'`
Expected: FAIL to compile — `undefined: WriteFrame`, `NewFrameReader`, `MaxFrameLen`, `FrameReader`, `FrameTooLargeError`.

- [ ] **Step 5: Implement `pubsub/frame.go`**

```go
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

// WriteFrame writes msg to w as one PubSubFrame. Header and payload go in ONE
// AppendData call: trsf serialises whole calls, so two publishers writing the
// same subscriber cannot interleave one's header with the other's payload.
func WriteFrame(w FrameWriter, msg []byte) error {
	if len(msg) >= MaxFrameLen {
		return fmt.Errorf("pubsub: message of %d bytes is at or over the %d-byte frame cap", len(msg), MaxFrameLen)
	}
	hdr := (&protocol.PubSubFrameHeader{DataLen: uint32(len(msg))}).MustAppend(nil)
	return w.AppendData(false, hdr, msg)
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
```

- [ ] **Step 6: Run the tests**

Run: `go test ./pubsub/ && GOOS=js GOARCH=wasm go build ./pubsub/...`
Expected: PASS; wasm build OK.

- [ ] **Step 7: Commit**

```bash
git add pubsub/protocol/message.bgn pubsub/protocol/message.go pubsub/frame.go pubsub/frame_test.go
git commit -m "feat(pubsub): PubSubFrame, and a writer and reader for it"
```

---

### Task 2: `OnSubscribe` hands a send function, not the stream

Still unframed: `send` writes raw. This task only removes the replay's access to the stream (D10), so Task 3's switch is a one-line change at the one place `send` is built.

**Files:**
- Modify: `pubsub/pubsub.go` (`OnSubscribe` field + doc, its call in `Subscribe`)
- Modify: `server/notify_replay.go`, `server/server.go:379-381`
- Test: `pubsub/pubsub_test.go` (`TestPubSub_OnSubscribeHookFires`), `server/notify_replay_test.go`

**Interfaces:**
- Produces: `PubSub.OnSubscribe func(topic string, send func(msg []byte) error)`; `func replayNotifications(ring *notifyRing, topic string, send func(msg []byte) error)`

- [ ] **Step 1: Update the tests to the new shape**

In `pubsub/pubsub_test.go`, `TestPubSub_OnSubscribeHookFires`: replace `hookStream trsf.BidirectionalStream` with `hookSend func([]byte) error`, the hook with

```go
	ps.OnSubscribe = func(topic string, send func([]byte) error) {
		hookCalls++
		hookTopic = topic
		hookSend = send
	}
```

and the nil check with

```go
	if hookSend == nil {
		t.Fatal("expected non-nil send in OnSubscribe hook")
	}
```

In `server/notify_replay_test.go`, delete `fakeStream`; collect sends instead:

```go
	var other [][]byte
	replayNotifications(r, "tasks.status", func(m []byte) error { other = append(other, m); return nil })
	if len(other) != 0 {
		t.Fatalf("replayed to wrong topic: %d writes", len(other))
	}

	var writes [][]byte
	replayNotifications(r, topics.Notifications(), func(m []byte) error { writes = append(writes, m); return nil })
	if len(writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(writes))
	}
```

and decode `writes[0]` / `writes[1]` where the test decoded `nf.writes[0]` / `nf.writes[1]`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./pubsub/ ./server/ -run 'OnSubscribeHookFires|ReplayNotifications'`
Expected: FAIL to compile — cannot use func literal as `func(string, trsf.BidirectionalStream)`; `replayNotifications` argument type mismatch.

- [ ] **Step 3: Implement**

`pubsub/pubsub.go`, the field:

```go
	// OnSubscribe, when non-nil, is called once per successful Subscribe, with
	// the joined topic and a function that sends one message to the new
	// subscriber, while the broker lock (ps.m) is held — so anything sent here
	// reaches the subscriber BEFORE any concurrent Publish to the same topic.
	// Used for replay-on-subscribe (the notifications ring backlog). It gets a
	// send function rather than the stream so that what it sends is a message
	// like any other, never bytes outside one. It must NOT call ps.Publish,
	// ps.Subscribe, or ps.Unsubscribe — ps.m is already held and is not
	// reentrant; doing so self-deadlocks.
	OnSubscribe func(topic string, send func(msg []byte) error)
```

In `Subscribe`, replace `ps.OnSubscribe(topic, stream)` with:

```go
		ps.OnSubscribe(topic, func(msg []byte) error { return stream.AppendData(false, msg) })
```

`server/notify_replay.go` becomes:

```go
package server

import (
	"github.com/on-keyday/agent-harness/topics"
)

// replayNotifications sends the ring backlog (oldest first) to a newly-joined
// subscriber of the notifications topic, so a client that connects later still
// sees recent notifications. No-op for any other topic. Each event is its own
// message.
func replayNotifications(ring *notifyRing, topic string, send func(msg []byte) error) {
	if ring == nil || topic != topics.Notifications() {
		return
	}
	for _, ev := range ring.snapshot() {
		// Best-effort: a closed/slow subscriber is not a replay failure, so the
		// error is intentionally ignored (mirrors pubsub.Publish).
		_ = send(ev.MustAppend(nil))
	}
}
```

`server/server.go`:

```go
	s.pubsub.OnSubscribe = func(topic string, send func([]byte) error) {
		replayNotifications(s.notifyRing, topic, send)
	}
```

Remove the `trsf` import from `server/server.go` only if nothing else in the file uses it (`go build` says so).

- [ ] **Step 4: Run the tests**

Run: `go build ./... && go test ./pubsub/ ./server/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pubsub/pubsub.go pubsub/pubsub_test.go server/notify_replay.go server/notify_replay_test.go server/server.go
git commit -m "refactor(pubsub): OnSubscribe sends messages instead of holding the stream"
```

---

### Task 3: The broker frames, every subscriber reads frames

One commit: producers and readers change together, so no commit has a server that frames and a reader that does not.

**Files:**
- Modify: `pubsub/pubsub.go` (`PublishFiltered`, `Subscribe`'s `send`)
- Modify: `tui/events.go` (`subscribeAndStream`, `SubscribeNotifications`, delete `drainNotifyEvents`; add `PumpEvents`)
- Modify: `cli/watch.go`, `cli/conns.go`, `cli/notify_watch.go`, `cli/logs.go`; Create: `cli/frames.go`
- Modify: `cmd/harness-webui-wasm/main.go` (the comment naming `drainTaskEvents` / `drainRunnerEvents`)
- Test: `tui/events_pump_test.go` (new), `tui/notify_test.go` (delete `TestDrainNotifyEventsCoalesced`), `cli/notify_watch_test.go` (replace `TestDrainNotifyEvents`), `pubsub/pubsub_test.go` (`TestPubSub_SlowSubscriberDoesNotStallOtherTopics`), `integration/pubsub_framing_e2e_test.go` (new)

**Interfaces:**
- Consumes: Task 1's `pubsub.WriteFrame`, `pubsub.NewFrameReader`, `pubsub.StreamReader`; Task 2's `OnSubscribe` shape.
- Produces:
  - `func tui.PumpEvents(ctx context.Context, st pubsub.StreamReader, topic string, fn func([]byte) tea.Msg, send func(tea.Msg)) error` — returns the reader's error (`io.EOF` on a clean end) or `ctx.Err()`.
  - `func cli.readFrames(st pubsub.StreamReader, topic string, each func(payload []byte)) error` (unexported) — same contract, no ctx.

- [ ] **Step 1: Extract the TUI's pump unchanged, so a test can drive it**

In `tui/events.go`, replace `subscribeAndStream`'s body with a call to a new exported function that holds the CURRENT loop (raw reads, one read = one message):

```go
// PumpEvents reads topic's subscriber stream and sends fn's result for each
// message. It returns when the stream ends or fails, or ctx is done.
func PumpEvents(ctx context.Context, st pubsub.StreamReader, topic string, fn func([]byte) tea.Msg, send func(tea.Msg)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return err
		}
		if len(data) > 0 {
			if msg := fn(data); msg != nil {
				send(msg)
			}
		}
		if eof {
			return io.EOF
		}
	}
}

func subscribeAndStream(ctx context.Context, c *cli.Client, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	resubscribeLoop(ctx, c, topic, program, func(st trsf.BidirectionalStream) {
		_ = PumpEvents(ctx, st, topic, fn, program.Send)
	})
}
```

(imports: `io`, `github.com/on-keyday/agent-harness/pubsub`). Run `go build ./tui/` — expect OK.

- [ ] **Step 2: Write the failing unit test**

Create `tui/events_pump_test.go`:

```go
package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type chunkStream struct{ chunks [][]byte }

func (s *chunkStream) ReadDirect(uint64) ([]byte, bool, error) {
	if len(s.chunks) == 0 {
		return nil, true, nil
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, len(s.chunks) == 0, nil
}

type frameSink struct{ wire []byte }

func (f *frameSink) AppendData(_ bool, data ...[]byte) error {
	for _, d := range data {
		f.wire = append(f.wire, d...)
	}
	return nil
}

func execEvent(t *testing.T, line string) []byte {
	t.Helper()
	var info protocol.ExecRunInfo
	info.Argv = protocol.ExecArgv{ArgvLen: 1, Argv: []protocol.ExecArg{{ArgLen: uint16(len(line)), Arg: []byte(line)}}}
	ev := protocol.ExecStatusEvent{Kind: protocol.StatusEventKind_ExecStarted, Ts: 1, Info: info}
	return ev.MustAppend(nil)
}

func execMsg(payload []byte) tea.Msg {
	ev, err := DecodeExecStatus(payload)
	if err != nil {
		return nil
	}
	return ExecStatusMsg{Event: ev}
}

// The live failure: an ExecStatusEvent with a 3 KiB argv split across reads
// reached the TUI as two halves, and neither decoded.
func TestPumpEvents_LongExecEventSplitAcrossReads(t *testing.T) {
	line := "echo " + strings.Repeat("a", 3000)
	var sink frameSink
	if err := pubsub.WriteFrame(&sink, execEvent(t, line)); err != nil {
		t.Fatal(err)
	}
	mid := len(sink.wire) / 2
	st := &chunkStream{chunks: [][]byte{sink.wire[:mid], sink.wire[mid:]}}

	var got []tea.Msg
	_ = PumpEvents(context.Background(), st, "execs.status", execMsg, func(m tea.Msg) { got = append(got, m) })

	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	ev := got[0].(ExecStatusMsg).Event
	if a := ev.Info.Argv.Argv; len(a) != 1 || string(a[0].Arg) != line {
		t.Fatalf("argv did not survive the split")
	}
}

// Two events in one read: the second was dropped without a word.
func TestPumpEvents_TwoEventsInOneRead(t *testing.T) {
	var sink frameSink
	for _, l := range []string{"one", "two"} {
		if err := pubsub.WriteFrame(&sink, execEvent(t, l)); err != nil {
			t.Fatal(err)
		}
	}
	var got []tea.Msg
	_ = PumpEvents(context.Background(), &chunkStream{chunks: [][]byte{sink.wire}}, "execs.status", execMsg,
		func(m tea.Msg) { got = append(got, m) })
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
}
```

- [ ] **Step 3: Run it to see it fail**

Run: `go test ./tui/ -run TestPumpEvents`
Expected: FAIL — `got 0 messages, want 1` and `got 0 messages, want 2` (each read is decoded as one event, and the frame header makes it undecodable).

- [ ] **Step 4: Write the failing integration test (the real path)**

Create `integration/pubsub_framing_e2e_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/tui"
)

// An exec whose argv spans several segments reaches an execs.status
// subscriber as whole events — the TUI logged
// `not enough data to read for field "ExecArg::Arg"` and dropped them.
func TestPubsubFraming_LongArgvExecEventReachesTheTUIWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the command is POSIX shell — skipping on Windows")
	}
	clearAgentEnv(t)

	wsCID, _, _ := startDualStackServer(t)
	repo := tempRepo(t)
	startRunner(t, wsCID, runnerOpts{MaxTasks: 2, Roots: []string{repo}, ClaudeBin: fakeClaudeSlowPath(t)})
	c := dialClient(t, wsCID)
	taskID := openLiveSession(t, c, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := c.Peer().JoinAndGetStream(ctx, "test", topics.ExecsStatus())
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu      sync.Mutex
		started []protocol.ExecStatusEvent
	)
	go tui.PumpEvents(ctx, st, topics.ExecsStatus(), func(p []byte) tea.Msg {
		ev, err := tui.DecodeExecStatus(p)
		if err != nil {
			t.Errorf("decode: %v", err)
			return nil
		}
		return tui.ExecStatusMsg{Event: ev}
	}, func(m tea.Msg) {
		ev := m.(tui.ExecStatusMsg).Event
		if ev.Kind == protocol.StatusEventKind_ExecStarted {
			mu.Lock()
			started = append(started, ev)
			mu.Unlock()
		}
	})

	line := "echo " + strings.Repeat("a", 8192) + " END"
	if _, err := c.ExecRun(ctx, taskID, []string{line}, cli.ExecRunOpts{ShellLine: true}); err != nil {
		t.Fatalf("ExecRun: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(started)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 {
		t.Fatalf("got %d exec_started events, want 1", len(started))
	}
	if a := started[0].Info.Argv.Argv; len(a) == 0 || !strings.HasSuffix(string(a[len(a)-1].Arg), " END") {
		t.Fatal("exec_started argv does not carry the whole line")
	}
}
```

A nil `Stdout` is fine: `cli/exec_run.go` copies through `copyIfSet`.

- [ ] **Step 5: Run it against the unframed code**

Run: `go test -tags integration ./integration/ -run TestPubsubFraming -count=1 -v 2>&1 | tail -30`
Expected: FAIL — decode errors naming `ExecArg::Arg`, or `got 0 exec_started events`. Loopback may deliver the whole event in one read; if it PASSES, record that in the ledger (the unit test in Step 3 is the deterministic RED) and continue.

- [ ] **Step 6: Frame the producers**

`pubsub/pubsub.go`, `PublishFiltered`:

```go
	for _, p := range toSend {
		if allow == nil || allow(p.id) {
			WriteFrame(p.conn, msg) //nolint:errcheck
		}
	}
```

and `Subscribe`'s hook:

```go
		ps.OnSubscribe(topic, func(msg []byte) error { return WriteFrame(stream, msg) })
```

Update `PublishFiltered`'s doc comment to say each message is written as one `PubSubFrame`. In `pubsub/pubsub_test.go` `TestPubSub_SlowSubscriberDoesNotStallOtherTopics`, `recordingStream.AppendData` now receives `[header, payload]`; compare the LAST slice of the call:

```go
func (r *recordingStream) AppendData(eof bool, data ...[]byte) error {
	if len(data) > 0 {
		select {
		case r.got <- data[len(data)-1]:
		default:
		}
	}
	return nil
}
```

- [ ] **Step 7: Read frames in every subscriber**

`tui/events.go` — `PumpEvents` reads frames:

```go
// PumpEvents reads topic's subscriber stream one message at a time and sends
// fn's result for each. It returns the reader's error — io.EOF when the stream
// ended cleanly — or ctx.Err().
func PumpEvents(ctx context.Context, st pubsub.StreamReader, topic string, fn func([]byte) tea.Msg, send func(tea.Msg)) error {
	fr := pubsub.NewFrameReader(st)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		payload, err := fr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				slog.Warn("subscription stream", "topic", topic, "err", err)
			}
			return err
		}
		if msg := fn(payload); msg != nil {
			send(msg)
		}
	}
}
```

`SubscribeNotifications` becomes a `subscribeAndStream` call, and `drainNotifyEvents` is deleted:

```go
// SubscribeNotifications joins the notifications topic and forwards each decoded
// NotifyEvent as NotifyEventMsg. The ring replay arrives first, as ordinary
// messages.
func SubscribeNotifications(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.Notifications(), program, func(payload []byte) tea.Msg {
		var ev protocol.NotifyEvent
		if _, err := ev.Decode(payload); err != nil {
			slog.Warn("decode notify event", "err", err)
			return nil
		}
		return NotifyEventMsg{Event: ev}
	})
}
```

Fix `subscribeAndStream`'s doc comment (it mentions a "header-discard dance"): it joins, then pumps messages through `fn` via `PumpEvents`. Delete `TestDrainNotifyEventsCoalesced` from `tui/notify_test.go` (Task 1 and Step 2 cover what it covered).

Create `cli/frames.go`:

```go
package cli

import (
	"errors"
	"io"
	"log/slog"

	"github.com/on-keyday/agent-harness/pubsub"
)

// readFrames calls each with every message on a subscriber stream, in order,
// and returns when the stream ends (nil) or fails. A payload each cannot use is
// each's to report; the next message is unaffected, since frames delimit them.
func readFrames(st pubsub.StreamReader, topic string, each func(payload []byte)) error {
	fr := pubsub.NewFrameReader(st)
	for {
		payload, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			slog.Warn("subscription stream", "topic", topic, "err", err)
			return err
		}
		each(payload)
	}
}
```

`cli/watch.go` — `consumeTasks` / `consumeRunners` become:

```go
	consumeTasks := func(st trsf.BidirectionalStream) {
		_ = readFrames(st, tasksTopic, func(p []byte) {
			ev := &protocol.TaskStatusEvent{}
			if _, err := ev.Decode(p); err != nil {
				slog.Warn("decode task event", "err", err)
				return
			}
			mu.Lock()
			fmt.Fprintf(out, "task %x kind=%s status=%s ts=%d exit=%d\n",
				ev.TaskId.Id[:6], ev.Kind, ev.TaskStatus, ev.Ts, ev.ExitCode)
			mu.Unlock()
		})
	}

	consumeRunners := func(st trsf.BidirectionalStream) {
		_ = readFrames(st, runnersTopic, func(p []byte) {
			ev := &protocol.RunnerStatusEvent{}
			if _, err := ev.Decode(p); err != nil {
				slog.Warn("decode runner event", "err", err)
				return
			}
			mu.Lock()
			fmt.Fprintf(out, "runner kind=%s status=%s ts=%d\n", ev.Kind, ev.RunnerStatus, ev.Ts)
			mu.Unlock()
		})
	}
```

and delete `drainTaskEvents` / `drainRunnerEvents`.

`cli/conns.go` — the goroutine in `watchConnStatusEvents`:

```go
	go func() {
		_ = readFrames(stream, topic, func(p []byte) {
			ev := &protocol.ConnStatusEvent{}
			if _, err := ev.Decode(p); err != nil {
				slog.Warn("decode conn event", "err", err)
				return
			}
			mu.Lock()
			fmt.Fprintln(out, line(ev))
			mu.Unlock()
		})
	}()
```

and delete `drainConnStatusEvents`.

`cli/notify_watch.go` — the goroutine in `watchNotifications`:

```go
	go func() {
		_ = readFrames(stream, topic, func(p []byte) {
			ev := &protocol.NotifyEvent{}
			if _, err := ev.Decode(p); err != nil {
				slog.Warn("decode notify event", "err", err)
				return
			}
			mu.Lock()
			fmt.Fprintln(out, line(ev))
			mu.Unlock()
		})
	}()
```

and delete `drainNotifyEvents`.

`cli/logs.go` — the follow loop after `JoinAndGetStream` (explicit, not `readFrames`, because a write error must end `Logs`):

```go
	fr := pubsub.NewFrameReader(st)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		p, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, werr := out.Write(p); werr != nil {
			return werr
		}
	}
```

`cli/notify_watch_test.go` — replace `TestDrainNotifyEvents` with a test of the reader the watcher now uses:

```go
type chunkStream struct{ chunks [][]byte }

func (s *chunkStream) ReadDirect(uint64) ([]byte, bool, error) {
	if len(s.chunks) == 0 {
		return nil, true, nil
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, len(s.chunks) == 0, nil
}

type frameSink struct{ wire []byte }

func (f *frameSink) AppendData(_ bool, data ...[]byte) error {
	for _, d := range data {
		f.wire = append(f.wire, d...)
	}
	return nil
}

// A payload that does not decode is skipped; the events around it still arrive.
func TestReadFrames_BadPayloadDoesNotDisturbTheNext(t *testing.T) {
	e1 := (&protocol.NotifyEvent{Ts: 1, Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_External, TextLen: 2, Text: []byte("hi")}).MustAppend(nil)
	e2 := (&protocol.NotifyEvent{Ts: 2, Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("x")}).MustAppend(nil)
	var sink frameSink
	for _, m := range [][]byte{e1, {0xff}, e2} {
		if err := pubsub.WriteFrame(&sink, m); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	err := readFrames(&chunkStream{chunks: [][]byte{sink.wire[:5], sink.wire[5:]}}, "notifications", func(p []byte) {
		ev := &protocol.NotifyEvent{}
		if _, err := ev.Decode(p); err != nil {
			return
		}
		lines = append(lines, notifyEventJSONLine(ev))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], `"level":"info"`) || !strings.Contains(lines[1], `"level":"warn"`) {
		t.Fatalf("lines = %q, want the info event then the warn event", lines)
	}
}
```

(imports: drop `bytes` / `sync` if unused; add `github.com/on-keyday/agent-harness/pubsub`.)

`cmd/harness-webui-wasm/main.go` — the `watchPipe` comment: replace `(see drainTaskEvents / drainRunnerEvents in cli/watch.go)` with `(see Watch in cli/watch.go)`.

- [ ] **Step 8: Run everything**

Run: `go build ./... && go test ./pubsub/ ./tui/ ./cli/ ./server/ && go test -tags integration ./integration/ -run 'TestPubsubFraming|TestExecRun|Notif|Watch|Logs' -count=1 2>&1 | tail -20 && make vet && make wasm-check && grep -rn "drainNotifyEvents\|drainTaskEvents\|drainRunnerEvents\|drainConnStatusEvents" --include=*.go .`
Expected: all PASS; the final grep prints nothing.

Then the whole suite: `make test > "$WS/make-test.log" 2>&1; tail -5 "$WS/make-test.log"` and `go test -tags integration ./integration/ -count=1 > "$WS/integ.log" 2>&1; tail -5 "$WS/integ.log"` (`$WS` = this plan's sdd workspace).
Expected: `ok` for every package.

- [ ] **Step 9: Commit**

```bash
git add pubsub/ tui/ cli/ cmd/harness-webui-wasm/main.go integration/pubsub_framing_e2e_test.go
git commit -m "feat(pubsub): the broker frames every message to a subscriber, and every subscriber reads frames"
```

---

### Task 4: Live verification

No new code unless a check fails (a failure goes back to the owning task's file, with a test first).

- [ ] **Step 1: wire-skew-check**

Run: `scripts/wire-skew-check.sh`
Expected: all three phases PASS (runner × server is unchanged by this plan; the run confirms it).

- [ ] **Step 2: Dummy harness, on the built binaries of this worktree**

Run: `make build`, then `scripts/dummy-harness.sh up --detach --agent fake`, then `eval "$(scripts/dummy-harness.sh env)"`, and use `$PWD/bin/harness-cli` and `$PWD/bin/harness-tui` (a bare `harness-cli` on PATH is the parent checkout's build).

1. Open an interactive `bash` session; in a second shell start `bin/harness-tui` inside a session sized with `session send --resize 45x200`, with `HARNESS_LOG`/stderr captured to a file.
2. `bin/harness-cli exec <task> -- sh -c "echo $(printf 'a%.0s' $(seq 3000)) END"` (a ~3 KiB argv).
3. Snapshot the TUI's exec pane: the exec row appears, then disappears on `exec_ended`. The TUI log has no `decode exec event` WARN.
4. `bin/harness-cli watch` prints task lines while a oneshot runs; `bin/harness-cli logs -f <task>` streams and exits when the task ends; `bin/harness-cli notify-watch` prints the ring backlog.
5. The WebUI (served from `webui/` by the dummy) shows the task list updating and a notification arriving.

Expected: all five as stated. Then `scripts/dummy-harness.sh down`.

- [ ] **Step 3: Record**

Append to the ledger what each check showed (command + result). No commit unless a fix was needed.
