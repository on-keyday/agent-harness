package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// Untagged, like cli/snapshot_raw.go and for the same reason: everything here
// goes through attachSessionRPC + agentexec.CommandExecutionStream, which the
// js and native builds share. The WebUI increment needs these under
// GOOS=js, so putting them behind !js now would only have to be undone.

// EncodeStreamMsg marshals one adapter-protocol message and appends the newline
// that frames it.
//
// THE ONE builder for this grammar (surface-parity checklist 32): the CLI verbs
// and the TUI chat both go through it, so neither can drift into its own
// spelling of a message the adapter has to parse. The newline is the part that
// must not be re-derived — measured on a live session, a line without it sits
// in the adapter's line buffer until some later write flushes it, so the
// operator sees nothing happen and gets no error either.
//
// `session send` deliberately does NOT go through here: it is the raw-bytes
// escape hatch and appends nothing, which is what lets it put a deliberately
// malformed line on the stream.
func EncodeStreamMsg(m streamagent.Msg) ([]byte, error) {
	if m.V == 0 {
		m.V = streamagent.ProtocolVersion
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", m.Kind, err)
	}
	return append(b, '\n'), nil
}

// openStreamAttach attaches in cowrite mode and refuses a task whose data plane
// is not NDJSON, so a write verb cannot type JSON into somebody's shell. The
// stream is closed before the error is returned.
func (c *Client) openStreamAttach(ctx context.Context, taskIDHex string, what string) (*agentexec.CommandExecutionStream, error) {
	st, ar, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_Cowrite, 0)
	if err != nil {
		return nil, err
	}
	stream := agentexec.NewCommandExecutionStream(st)
	if ar.Kind != protocol.TaskKind_Stream {
		_ = stream.Close()
		return nil, fmt.Errorf("task %s is not an event-stream session (kind %s): %s has no meaning there: %w",
			taskIDHex, ar.Kind, what, ErrAttachWrongKind)
	}
	return stream, nil
}

// writeStreamMsg is the short-lived write: one cowrite attach, one line, drain,
// detach. It is what every `session stream <verb>` CLI call does. A surface
// that both follows and drives holds a StreamSession instead — reattaching per
// keystroke would re-replay the ring every time.
func (c *Client) writeStreamMsg(ctx context.Context, taskIDHex string, m streamagent.Msg, flush time.Duration) error {
	line, err := EncodeStreamMsg(m)
	if err != nil {
		return err
	}
	stream, err := c.openStreamAttach(ctx, taskIDHex, "`session stream "+string(m.Kind)+"`")
	if err != nil {
		return err
	}
	defer stream.Close()
	if _, err := stream.Stdin().Write(line); err != nil {
		return err
	}
	// Same reason SessionSend drains: Close cancels the underlying transport,
	// so closing immediately after the write can drop the in-flight frame.
	if flush > 0 {
		t := time.NewTimer(flush)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return nil
}

// StreamTurn appends one user turn to an event-stream task.
func (c *Client) StreamTurn(ctx context.Context, taskIDHex, text string, flush time.Duration) error {
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindUser, User: &streamagent.UserTurn{Text: text},
	}, flush)
}

// StreamApprove answers a pending request.
//
// r.ID is the staleness guard: the adapter refuses an id it is not holding, so
// a stale answer is rejected rather than applied to whatever happens to be
// pending (design §3). A deny's Message reaches the AGENT verbatim as a failed
// tool result — operator-authored text entering a model's context, not a
// private audit note.
func (c *Client) StreamApprove(ctx context.Context, taskIDHex string, r streamagent.Response, flush time.Duration) error {
	if r.ID == "" {
		return fmt.Errorf("approve: a request id is required — it is what makes a stale answer a refusal rather than a misapplied one")
	}
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindResponse, Response: &r,
	}, flush)
}

// StreamInterrupt abandons the running turn. The agent survives and takes the
// next turn normally; this is not a kill, and it is not a finish.
func (c *Client) StreamInterrupt(ctx context.Context, taskIDHex string, flush time.Duration) error {
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{},
	}, flush)
}

// StreamFinish ends the session cleanly: the adapter closes the agent's stdin,
// so it completes the turn in flight and exits 0.
func (c *Client) StreamFinish(ctx context.Context, taskIDHex string, flush time.Duration) error {
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindFinish, Finish: &streamagent.Finish{},
	}, flush)
}

// StreamLine is one line read off an event-stream task. Decoded=false with Raw
// set is a line that is not the protocol — `session send` can put one there —
// and it is carried rather than dropped: a follower who cannot see what a
// cowriter injected cannot explain what the adapter does next.
type StreamLine struct {
	Msg     streamagent.Msg
	Raw     []byte
	Decoded bool
}

// decodeStreamLine never fails on content. The error return exists for the
// shape callers expect and is always nil today; an undecodable line is data,
// not a fault.
func decodeStreamLine(line []byte) (StreamLine, error) {
	out := StreamLine{Raw: append([]byte(nil), line...)}
	m, err := streamagent.DecodeMsg(line)
	if err != nil {
		return out, nil
	}
	out.Msg, out.Decoded = m, true
	return out, nil
}

// StreamSession is a live COWRITE attach to an event-stream task: it reads the
// adapter's messages and writes the client's, on ONE attach. Cowrite takes no
// writer seat, so several may coexist and the task stays Detached — which for
// this kind is the ordinary state, not an anomaly.
//
// The short-lived Stream* helpers above open one attach per call, which is
// right for a CLI verb that exits. A surface that both follows and drives (the
// TUI chat) holds one of these instead.
type StreamSession struct {
	stream *agentexec.CommandExecutionStream
	rd     *bufio.Reader
	// wmu serialises writes: ReadLine runs on its own goroutine, so a Send from
	// the UI goroutine would otherwise interleave with nothing protecting the
	// underlying stream.
	wmu sync.Mutex
}

// OpenStreamSession attaches to an event-stream task for both directions.
func (c *Client) OpenStreamSession(ctx context.Context, taskIDHex string) (*StreamSession, error) {
	stream, err := c.openStreamAttach(ctx, taskIDHex, "the chat view")
	if err != nil {
		return nil, err
	}
	return &StreamSession{stream: stream, rd: bufio.NewReader(stream.Stdout())}, nil
}

// ReadLine returns the next line off the stream. It BLOCKS; run it on its own
// goroutine. A final partial line (no trailing newline) is still returned,
// alongside the error that ended the stream.
func (s *StreamSession) ReadLine() (StreamLine, error) {
	line, err := s.rd.ReadBytes('\n')
	if len(line) > 0 {
		out, _ := decodeStreamLine(bytes.TrimRight(line, "\r\n"))
		return out, err
	}
	return StreamLine{}, err
}

// Stderr is the AGENT's stderr, which rides its own frame type and is not
// NDJSON. A caller that does not drain it backpressures the whole stream, so
// treat this as required rather than optional — `session stream attach` copies
// it on its own goroutine for exactly that reason.
func (s *StreamSession) Stderr() io.Reader { return s.stream.Stderr() }

// Send writes one message. Safe from any goroutine.
func (s *StreamSession) Send(m streamagent.Msg) error {
	b, err := EncodeStreamMsg(m)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stream.Stdin().Write(b)
	return err
}

// Turn, Approve, Interrupt and Finish are Send with the message built, so a
// surface never assembles a streamagent.Msg by hand — same rule as the
// short-lived helpers, same one builder underneath.
func (s *StreamSession) Turn(text string) error {
	return s.Send(streamagent.Msg{Kind: streamagent.KindUser, User: &streamagent.UserTurn{Text: text}})
}

func (s *StreamSession) Approve(r streamagent.Response) error {
	if r.ID == "" {
		return fmt.Errorf("approve: a request id is required")
	}
	return s.Send(streamagent.Msg{Kind: streamagent.KindResponse, Response: &r})
}

func (s *StreamSession) Interrupt() error {
	return s.Send(streamagent.Msg{Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{}})
}

func (s *StreamSession) Finish() error {
	return s.Send(streamagent.Msg{Kind: streamagent.KindFinish, Finish: &streamagent.Finish{}})
}

func (s *StreamSession) Close() error { return s.stream.Close() }
