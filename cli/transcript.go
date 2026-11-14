package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// Agent transcripts (pillar B, docs/superpowers/specs/2026-11-14-pillar-b-transcripts-design.md):
// the runner resolves the task's working directory and each agent's store,
// and the answer — a TranscriptResult header, then for a get exactly
// get.size bytes — is spliced to the caller on a stream.

// CheckTranscriptNames refuses a session id or file name past the bounds
// (protocol.TranscriptSessionMax / TranscriptFileMax) before a request is built.
func CheckTranscriptNames(session, file string) error {
	if len(session) > protocol.TranscriptSessionMax {
		return fmt.Errorf("transcript: session id is %d bytes, at most %d", len(session), protocol.TranscriptSessionMax)
	}
	if len(file) > protocol.TranscriptFileMax {
		return fmt.Errorf("transcript: file name is %d bytes, at most %d", len(file), protocol.TranscriptFileMax)
	}
	return nil
}

func transcriptStatusError(verb string, st protocol.TranscriptStatus, msg []byte) error {
	what := EnumWireName(st.String())
	switch st {
	case protocol.TranscriptStatus_Ok:
		return nil
	case protocol.TranscriptStatus_NoSuchTask:
		what += ": no such task, or it is outside your transcript_read scope"
	case protocol.TranscriptStatus_RunnerOffline:
		what += ": the task's runner is not connected (transcripts are read from it; none is kept on the server)"
	case protocol.TranscriptStatus_RunnerUnsupported:
		what += ": the task's runner predates transcripts; rebuild and restart it"
	}
	if len(msg) > 0 {
		what += ": " + string(msg)
	}
	return fmt.Errorf("transcript %s: %s", verb, what)
}

// readTranscriptAnswer reads the runner's answer from r. For a get it copies
// exactly the announced bytes to w and reports a stream that ends sooner as a
// cut, never as a complete file (review of Task 1, I2).
func readTranscriptAnswer(verb string, r io.Reader, w io.Writer) (protocol.TranscriptResult, error) {
	var res protocol.TranscriptResult
	if err := res.Read(r); err != nil {
		return res, fmt.Errorf("transcript %s: the answer was cut before its header: %w", verb, err)
	}
	if err := transcriptStatusError(verb, res.Status, res.Message); err != nil {
		return res, err
	}
	if res.Op != protocol.TranscriptOp_Get {
		return res, nil
	}
	g := res.Get()
	if g == nil {
		return res, fmt.Errorf("transcript %s: a get answer without its size", verb)
	}
	n, err := io.CopyN(w, r, int64(g.Size))
	if err != nil {
		if errors.Is(err, io.EOF) {
			return res, fmt.Errorf("transcript %s: the stream was cut after %d of %d bytes", verb, n, g.Size)
		}
		return res, fmt.Errorf("transcript %s: %w", verb, err)
	}
	return res, nil
}

// transcriptAnswerWait bounds the wait for the server's response. The server
// answers at once and the runner's work arrives on the stream, which has no
// such bound: a whole session's tar may take long to come.
const transcriptAnswerWait = 15 * time.Second

// awaitTranscriptAnswer runs rt with the response bound. A server older than
// transcripts drops the request it cannot decode and never answers, so the
// bound running out is named as that, not as a bare deadline.
func awaitTranscriptAnswer(ctx context.Context, verb string, wait time.Duration, rt func(context.Context) (*protocol.TaskControlResponse, error)) (*protocol.TaskControlResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := rt(rctx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("transcript %s: no answer from the server in %s — a server older than transcripts drops the request", verb, wait)
	}
	return resp, err
}

// streamCtxReader reads the stream under ctx, so a cancel (Ctrl-C, a closed
// sheet) ends the read; the caller's CloseBoth then fails the runner's write.
type streamCtxReader struct {
	ctx context.Context
	st  trsf.BidirectionalStream
}

func (r streamCtxReader) Read(p []byte) (int, error) { return r.st.ReadContext(r.ctx, p) }

// transcriptRoundTrip sends one request and reads the answer on its stream.
func (c *Client) transcriptRoundTrip(ctx context.Context, verb string, req *protocol.TaskControlRequest, w io.Writer) (protocol.TranscriptResult, error) {
	resp, err := awaitTranscriptAnswer(ctx, verb, transcriptAnswerWait, func(ctx context.Context) (*protocol.TaskControlResponse, error) {
		return c.RoundTripTaskControl(ctx, req)
	})
	if err != nil {
		return protocol.TranscriptResult{}, err
	}
	var r *protocol.TranscriptResponse
	switch {
	case resp.Kind != req.Header.Kind:
		return protocol.TranscriptResult{}, fmt.Errorf("transcript %s: unexpected response kind %v", verb, resp.Kind)
	case req.Header.Kind == protocol.TaskControlKind_TranscriptList:
		r = resp.TranscriptList()
	default:
		r = resp.TranscriptGet()
	}
	if r == nil {
		return protocol.TranscriptResult{}, fmt.Errorf("transcript %s: empty response", verb)
	}
	if err := transcriptStatusError(verb, r.Status, nil); err != nil {
		return protocol.TranscriptResult{}, err
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return protocol.TranscriptResult{}, fmt.Errorf("transcript %s: stream %d not visible after the response", verb, r.StreamId)
	}
	defer st.CloseBoth()
	if err := st.AppendData(true); err != nil {
		return protocol.TranscriptResult{}, fmt.Errorf("transcript %s: half-close: %w", verb, err)
	}
	return readTranscriptAnswer(verb, bufio.NewReaderSize(streamCtxReader{ctx, st}, 64<<10), w)
}

// TranscriptSessions lists the sessions the agents stored for the task's
// working directory, and Message names any store that could not be read.
func (c *Client) TranscriptSessions(ctx context.Context, taskIDHex string) ([]protocol.TranscriptSession, string, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, "", err
	}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TranscriptList}}
	req.SetTranscriptList(protocol.TranscriptListRequest{TaskId: tid})
	res, err := c.transcriptRoundTrip(ctx, "ls", req, io.Discard)
	if err != nil {
		return nil, "", err
	}
	var rows []protocol.TranscriptSession
	if b := res.Sessions(); b != nil {
		rows = b.Sessions
	}
	return rows, string(res.Message), nil
}

// TranscriptFiles lists the files one session is made of.
func (c *Client) TranscriptFiles(ctx context.Context, taskIDHex, session string) ([]protocol.TranscriptFile, error) {
	if err := CheckTranscriptNames(session, ""); err != nil {
		return nil, err
	}
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, err
	}
	body := protocol.TranscriptListRequest{TaskId: tid}
	body.SetSession([]byte(session))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TranscriptList}}
	req.SetTranscriptList(body)
	res, err := c.transcriptRoundTrip(ctx, "ls", req, io.Discard)
	if err != nil {
		return nil, err
	}
	if b := res.Files(); b != nil {
		return b.Files, nil
	}
	return nil, nil
}

// TranscriptGet writes one file of a session to w, as the agent stored it, or
// the whole session as a tar when file is empty.
func (c *Client) TranscriptGet(ctx context.Context, taskIDHex, session, file string, w io.Writer) error {
	if err := CheckTranscriptNames(session, file); err != nil {
		return err
	}
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return err
	}
	body := protocol.TranscriptGetRequest{TaskId: tid}
	body.SetSession([]byte(session))
	body.SetFile([]byte(file))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TranscriptGet}}
	req.SetTranscriptGet(body)
	_, err = c.transcriptRoundTrip(ctx, "get", req, w)
	return err
}

// TranscriptSessionRow is one session as `transcript ls` prints it.
func TranscriptSessionRow(s protocol.TranscriptSession) string {
	size := "unknown"
	if s.SizeKnown() {
		size = humanBytes(s.Size)
	}
	row := fmt.Sprintf("%s %s  created=%s  updated=%s  size=%s",
		s.Agent, s.Id, recordTime(s.CreatedNs), recordTime(s.UpdatedNs), size)
	if s.Shared() {
		row += "  shared"
	}
	return row
}

// TranscriptFileRow is one file of a session as `transcript ls <task> <session>` prints it.
func TranscriptFileRow(f protocol.TranscriptFile) string {
	size := "unknown (produced on read)"
	if f.SizeKnown() {
		size = humanBytes(f.Size)
	}
	return fmt.Sprintf("%s  size=%s", f.Name, size)
}

// TranscriptLsWith prints a task's sessions, or one session's files. A store
// that could not be read is named after the rows, never dropped silently.
func TranscriptLsWith(ctx context.Context, c *Client, taskIDHex, session string, out io.Writer) error {
	if session != "" {
		fs, err := c.TranscriptFiles(ctx, taskIDHex, session)
		if err != nil {
			return err
		}
		for _, f := range fs {
			fmt.Fprintln(out, TranscriptFileRow(f))
		}
		return nil
	}
	rows, msg, err := c.TranscriptSessions(ctx, taskIDHex)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no transcripts")
	}
	for _, r := range rows {
		fmt.Fprintln(out, TranscriptSessionRow(r))
	}
	if msg != "" {
		fmt.Fprintln(out, "note: "+msg)
	}
	return nil
}

// TranscriptGetWith writes one file, or the session's tar, to out.
func TranscriptGetWith(ctx context.Context, c *Client, taskIDHex, session, file string, out io.Writer) error {
	return c.TranscriptGet(ctx, taskIDHex, session, file, out)
}

// LazyFile creates its file only when the first byte is written, so a refused
// or failed fetch leaves no empty file behind.
type LazyFile struct {
	path string
	f    *os.File
}

func NewLazyFile(path string) *LazyFile { return &LazyFile{path: path} }

func (l *LazyFile) Write(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.Create(l.path)
		if err != nil {
			return 0, err
		}
		l.f = f
	}
	return l.f.Write(p)
}

func (l *LazyFile) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

// Opened reports whether anything was written (and so the file exists).
func (l *LazyFile) Opened() bool { return l.f != nil }

// TranscriptToFile runs fetch into path, the way `transcript get -o` writes on
// every surface. The file is created on the first byte, so a refusal leaves
// none; a fetch that succeeds with no bytes (an empty file) leaves an empty
// file; and a fetch that fails after writing removes what it wrote, so a cut
// transfer never sits on disk looking complete.
func TranscriptToFile(path string, fetch func(io.Writer) error) error {
	f := NewLazyFile(path)
	err := fetch(f)
	if err == nil && !f.Opened() {
		_, err = f.Write(nil)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil && f.Opened() {
		_ = os.Remove(path)
	}
	return err
}

func transcriptDial(ctx context.Context, peerCID objproto.ConnectionID, fn func(*Client) error) error {
	return recordDial(ctx, peerCID, fn)
}

// TranscriptLs is `transcript ls` for the CLI, on a fresh connection.
func TranscriptLs(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex, session string, out io.Writer) error {
	return transcriptDial(ctx, peerCID, func(c *Client) error { return TranscriptLsWith(ctx, c, taskIDHex, session, out) })
}

// TranscriptGetOut is `transcript get` for the CLI, on a fresh connection.
func TranscriptGetOut(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex, session, file string, out io.Writer) error {
	return transcriptDial(ctx, peerCID, func(c *Client) error { return TranscriptGetWith(ctx, c, taskIDHex, session, file, out) })
}
