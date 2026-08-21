package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/trsf"
)

// These drive the REAL adapter binary against a fake agent, over a pipe pair
// standing in for the server-allocated stream. The transport under test is
// agentexec with ptyEnabled=false, so the test speaks the same frame protocol a
// client does — which is the point. The first version of this file tested a
// hand-rolled framing that only it and the code under test understood, and
// passed.

var (
	adapterOnce sync.Once
	adapterPath string
	adapterErr  error
)

func testAdapter(t *testing.T) string {
	t.Helper()
	adapterOnce.Do(func() {
		dir, err := os.MkdirTemp("", "stream-adapter")
		if err != nil {
			adapterErr = err
			return
		}
		adapterPath = filepath.Join(dir, "harness-stream-adapter")
		out, err := exec.Command("go", "build", "-o", adapterPath,
			"github.com/on-keyday/agent-harness/cmd/harness-stream-adapter").CombinedOutput()
		if err != nil {
			adapterErr = err
			t.Logf("build: %s", out)
		}
	})
	if adapterErr != nil {
		t.Fatalf("building the adapter: %v", adapterErr)
	}
	return adapterPath
}

// pipeStream is a trsf.BidirectionalStream over two pipes. Only the methods
// agentexec actually uses carry behaviour: AppendData to send, Read to receive,
// Cancel/CloseBoth to tear down.
type pipeStream struct {
	toClient   *io.PipeWriter
	fromClient *io.PipeReader
	closeOnce  sync.Once
	torn       atomic.Bool
}

func (s *pipeStream) AppendData(_ bool, data ...[]byte) error {
	for _, d := range data {
		if _, err := s.toClient.Write(d); err != nil {
			// After teardown the session is over by definition; a real trsf
			// stream does not turn "the session ended" into a write failure,
			// and surfacing it here made a CLEAN finish report an error.
			if s.torn.Load() {
				return nil
			}
			return err
		}
	}
	return nil
}

func (s *pipeStream) AppendDataContext(_ context.Context, fin bool, data ...[]byte) error {
	return s.AppendData(fin, data...)
}
func (s *pipeStream) Read(p []byte) (int, error) {
	n, err := s.fromClient.Read(p)
	if err != nil && s.torn.Load() {
		// A torn-down stream reads as EOF, not as a pipe error. handleInput
		// treats EOF as a clean end and anything else as a session failure, so
		// getting this wrong made a CLEAN finish report an error.
		return n, io.EOF
	}
	return n, err
}
func (s *pipeStream) ReadContext(_ context.Context, p []byte) (int, error) {
	return s.fromClient.Read(p)
}
func (s *pipeStream) Write(p []byte) (int, error)                           { return len(p), s.AppendData(false, p) }
func (s *pipeStream) WriteContext(_ context.Context, p []byte) (int, error) { return s.Write(p) }
func (s *pipeStream) ID() trsf.StreamID                                     { return 1 }
func (s *pipeStream) Close() error                                          { return nil }
func (s *pipeStream) HasSendData() bool                                     { return false }
func (s *pipeStream) Completed() bool                                       { return false }
func (s *pipeStream) ReadDirect(uint64) ([]byte, bool, error)               { return nil, false, nil }
func (s *pipeStream) ReadDirectContext(context.Context, uint64) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *pipeStream) HasRecvData() bool { return false }
func (s *pipeStream) EOF() bool         { return false }
func (s *pipeStream) Cancel()           { s.teardown() }
func (s *pipeStream) CloseBoth() error  { s.teardown(); return nil }
func (s *pipeStream) teardown() {
	s.closeOnce.Do(func() {
		s.torn.Store(true)
		_ = s.toClient.CloseWithError(io.EOF)
		_ = s.fromClient.CloseWithError(io.EOF)
	})
}

// client is the other end: it unframes what the runner sends and frames what it
// sends back, as CommandExecutionStream does.
type client struct {
	out io.Reader
	in  *io.PipeWriter
}

func newStreamPair() (*pipeStream, *client) {
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	return &pipeStream{toClient: outW, fromClient: inR}, &client{out: outR, in: inW}
}

// nextStdout returns the payload of the next Stdout frame, skipping others.
func (c *client) nextStdout() ([]byte, error) {
	for {
		f := &frame.Frame{}
		if err := f.Read(c.out); err != nil {
			return nil, err
		}
		if f.Header.Type == frame.FrameType_Stdout {
			if d := f.Data(); d != nil {
				return *d, nil
			}
		}
	}
}

func (c *client) sendStdin(b []byte) error {
	hdr := frame.FrameHeader{Type: frame.FrameType_Stdin, Len: uint32(len(b))}
	_, err := c.in.Write(append(hdr.MustAppend(nil), b...))
	return err
}

// finish closes the agent's stdin without killing it: a zero-length Stdin
// frame, which is what CommandExecutionStream's stdin Close() sends. It is the
// clean end of an event-stream session, as distinct from a cancel — the
// mechanism the design had assumed did not exist.
func (c *client) finish() error {
	hdr := frame.FrameHeader{Type: frame.FrameType_Stdin, Len: 0}
	_, err := c.in.Write(hdr.MustAppend(nil))
	return err
}

func fakeStreamAgent(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agent.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The wiring question end to end: events reach the stream as Stdout frames, an
// approval reaches it, an answer sent as a Stdin frame reaches the agent, and
// the session ends cleanly on a zero-length Stdin frame rather than a kill.
func TestStreamTaskCarriesEventsAndApprovalsOverTheStream(t *testing.T) {
	agentStdin := filepath.Join(t.TempDir(), "agent-stdin")
	agent := fakeStreamAgent(t,
		`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}'`+"\n"+
			`printf '%s\n' '{"type":"control_request","request_id":"v-1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x"}}}'`+"\n"+
			// head -n 1, not cat: cat holds the file open until its stdin
			// closes, so "the answer arrived" would only be observable during
			// teardown — exactly when the test wants to stop.
			"head -n 1 > "+agentStdin+"\n"+
			`printf '%s\n' '{"type":"result","subtype":"success","duration_ms":5,"total_cost_usd":0}'`+"\n"+
			"cat > /dev/null\n")

	stream, cl := newStreamPair()

	var mu sync.Mutex
	var logLines []string
	var pendingSeen []int

	task := &StreamTask{
		AdapterPath: testAdapter(t),
		AgentArgv:   []string{agent},
		Dir:         t.TempDir(),
		LogSink: func(b []byte) {
			mu.Lock()
			defer mu.Unlock()
			logLines = append(logLines, strings.TrimRight(string(b), "\n"))
		},
		OnPending: func(n int) {
			mu.Lock()
			defer mu.Unlock()
			pendingSeen = append(pendingSeen, n)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- task.Run(ctx, stream) }()

	var sawHello, sawText, sawFinish bool
	var req *streamagent.Request
	deadline := time.Now().Add(45 * time.Second)
	for !sawFinish && time.Now().Before(deadline) {
		payload, err := cl.nextStdout()
		if err != nil {
			t.Fatalf("stream ended early (hello=%v text=%v req=%v): %v",
				sawHello, sawText, req != nil, err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
			if line == "" {
				continue
			}
			m, err := streamagent.DecodeMsg([]byte(line))
			if err != nil {
				t.Fatalf("undecodable line on the stream: %q (%v)", line, err)
			}
			switch m.Kind {
			case streamagent.KindHello:
				sawHello = true
			case streamagent.KindEvent:
				if m.Event.Kind == streamagent.EventText && m.Event.Text == "working" {
					sawText = true
				}
				if m.Event.Kind == streamagent.EventFinish {
					sawFinish = true
				}
			case streamagent.KindRequest:
				req = m.Request
				if p := task.Pending(); len(p) != 1 || p[0].ID != m.Request.ID {
					t.Errorf("Pending() = %+v, want the one request", p)
				}
				resp, _ := json.Marshal(streamagent.Msg{
					V: streamagent.ProtocolVersion, Kind: streamagent.KindResponse,
					Response: &streamagent.Response{ID: m.Request.ID, Behavior: streamagent.BehaviorAllow},
				})
				if err := cl.sendStdin(append(resp, '\n')); err != nil {
					t.Fatalf("sending the answer: %v", err)
				}
			}
		}
	}

	if !sawHello || !sawText || req == nil || !sawFinish {
		t.Fatalf("hello=%v text=%v request=%v finish=%v", sawHello, sawText, req != nil, sawFinish)
	}
	if req.Tool != "Write" {
		t.Errorf("request tool = %q", req.Tool)
	}

	// End it the CLEAN way: close the agent's stdin, not a cancel.
	if err := cl.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	// Keep draining. The adapter still writes its `exit` line, and a reader
	// that stops early blocks the writer — which looked exactly like a
	// teardown deadlock the first time.
	go func() {
		for {
			if _, err := cl.nextStdout(); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cleanly finished session reported an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after a zero-length Stdin frame closed the agent's stdin")
	}

	got, err := os.ReadFile(agentStdin)
	if err != nil {
		t.Fatalf("the agent never read its stdin: %v", err)
	}
	if !strings.Contains(string(got), `"type":"control_response"`) ||
		!strings.Contains(string(got), `"request_id":"v-1"`) {
		t.Errorf("the answer did not reach the agent as a vendor response:\n%s", got)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logLines, "\n")
	if !strings.Contains(joined, "[out]working") {
		t.Errorf("the agent's text was not rendered into the task log:\n%s", joined)
	}
	if !strings.Contains(joined, "approval needed") {
		t.Errorf("a blocked approval left no trace in the task log:\n%s", joined)
	}
	if strings.Contains(joined, `"kind":"event"`) {
		t.Errorf("neutral JSON leaked into the task log instead of a rendered line:\n%s", joined)
	}
	if len(pendingSeen) == 0 || pendingSeen[0] != 1 || pendingSeen[len(pendingSeen)-1] != 0 {
		t.Errorf("pending counts = %v, want 1 then back to 0", pendingSeen)
	}
}

// A failing agent must not come back as silence. Until objtrsf was fixed today
// this returned nil no matter what the child did, which is how every
// interactive task in this repo reported Succeeded.
func TestStreamTaskReportsAFailingAgent(t *testing.T) {
	agent := fakeStreamAgent(t, "exit 3\n")
	stream, cl := newStreamPair()
	task := &StreamTask{
		AdapterPath: testAdapter(t),
		AgentArgv:   []string{agent},
		Dir:         t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- task.Run(ctx, stream) }()
	go func() {
		for {
			if _, err := cl.nextStdout(); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a session whose agent exited 3 came back nil; the outcome " +
				"is being dropped again")
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("err = %v (%T), want an *exec.ExitError", err, err)
		}
		if ee.ExitCode() == 0 {
			t.Errorf("exit code = 0 for a failed session")
		}
	case <-time.After(45 * time.Second):
		t.Fatal("Run did not return")
	}

	// The AGENT's own code (as opposed to the adapter's) is only visible
	// because the adapter reports it in-band.
	if ex := task.Exit(); ex == nil {
		t.Error("the adapter reported no exit in-band")
	} else if ex.Code != 3 {
		t.Errorf("adapter reported exit %d, want the agent's 3", ex.Code)
	}
}

// An unresolvable adapter must fail the task at start, not warn.
func TestStreamTaskFailsLoudlyOnAMissingAdapter(t *testing.T) {
	stream, _ := newStreamPair()
	task := &StreamTask{
		AdapterPath: filepath.Join(t.TempDir(), "does-not-exist"),
		AgentArgv:   []string{"true"},
		Dir:         t.TempDir(),
	}
	err := task.Run(context.Background(), stream)
	if err == nil {
		t.Fatal("a missing adapter did not fail the task")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error does not name the adapter: %v", err)
	}
}

// The neutral Event must survive agentlog → neutral → agentlog, or this kind's
// log lines silently say less than the oneshot kind's for the same agent
// output. The two mappers live in different packages and nothing else pins them
// together.
func TestEventRoundTripsThroughTheNeutralType(t *testing.T) {
	code := 3
	cases := []agentlog.Event{
		{Kind: agentlog.KindText, Text: "hello"},
		{Kind: agentlog.KindThinking, Text: "hmm"},
		{Kind: agentlog.KindSessionStart, Text: "sess-1"},
		{Kind: agentlog.KindToolStart, Tool: "Bash", Args: `{"command":"ls"}`},
		{Kind: agentlog.KindToolEnd, Tool: "Bash", Result: "ok", ExitCode: &code},
		{Kind: agentlog.KindToolEnd, Tool: "Write", IsError: true},
		{Kind: agentlog.KindError, Text: "boom"},
		{Kind: agentlog.KindError, Text: "notice", Warning: true},
		{Kind: agentlog.KindFinish, Stats: agentlog.Stats{
			DurationMS: 12, CostUSD: 0.5, InputTokens: 7, OutputTokens: 9}},
		{Kind: agentlog.KindRaw, Text: "unparseable"},
	}
	for _, in := range cases {
		neutral := adapterToNeutral(in)
		var back streamagent.Event
		b, _ := json.Marshal(neutral)
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("neutral event does not survive JSON: %v", err)
		}
		if got := toAgentlog(back); agentlog.Render(got) != agentlog.Render(in) {
			t.Errorf("render drifted for %+v:\n  before %q\n  after  %q",
				in, agentlog.Render(in), agentlog.Render(got))
		}
	}
}

// adapterToNeutral reconstructs the adapter package's unexported mapper. The
// assertion above catches a divergence between the two as a render drift.
func adapterToNeutral(e agentlog.Event) streamagent.Event {
	out := streamagent.Event{
		Text: e.Text, Tool: e.Tool, Args: e.Args, Result: e.Result,
		ExitCode: e.ExitCode, IsError: e.IsError, Warning: e.Warning,
	}
	switch e.Kind {
	case agentlog.KindSessionStart:
		out.Kind = streamagent.EventSessionStart
	case agentlog.KindThinking:
		out.Kind = streamagent.EventThinking
	case agentlog.KindToolStart:
		out.Kind = streamagent.EventToolStart
	case agentlog.KindToolEnd:
		out.Kind = streamagent.EventToolEnd
	case agentlog.KindText:
		out.Kind = streamagent.EventText
	case agentlog.KindFinish:
		out.Kind = streamagent.EventFinish
	case agentlog.KindError:
		out.Kind = streamagent.EventError
	default:
		out.Kind = streamagent.EventRaw
	}
	if e.Stats != (agentlog.Stats{}) {
		out.Stats = &streamagent.Stats{
			DurationMS: e.Stats.DurationMS, CostUSD: e.Stats.CostUSD,
			InputTokens: e.Stats.InputTokens, OutputTokens: e.Stats.OutputTokens,
		}
	}
	return out
}

// Events reach BOTH the stream and the task log, and that is deliberate.
// `Detached` is a normal state for this kind, so the stream alone loses what
// happened while nobody was attached.
//
// An earlier version of this test asserted the opposite — that nothing reaches
// the log — because publishing opened a capability hole: reading the stream
// needs exec_view while GetTaskLog was gated on visibility alone. The hole was
// real and is fixed at its source instead; GetTaskLog now requires exec_view
// for every kind, so the two paths carry the same payload under the same gate.
func TestStreamTaskRendersEventsIntoTheTaskLog(t *testing.T) {
	agent := fakeStreamAgent(t,
		`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"recorded"}]}}'`+"\n"+
			"cat > /dev/null\n")
	stream, cl := newStreamPair()

	var logged []string
	var mu sync.Mutex
	task := &StreamTask{
		AdapterPath: testAdapter(t),
		AgentArgv:   []string{agent},
		Dir:         t.TempDir(),
		LogSink: func(b []byte) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, strings.TrimRight(string(b), "\n"))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx, stream) }()

	var sawOnStream bool
	deadline := time.Now().Add(30 * time.Second)
	for !sawOnStream && time.Now().Before(deadline) {
		payload, err := cl.nextStdout()
		if err != nil {
			break
		}
		if strings.Contains(string(payload), "recorded") {
			sawOnStream = true
		}
	}
	_ = cl.finish()
	// Keep reading. The adapter still writes its exit line, and a reader that
	// stops early blocks the writer — which looks exactly like a teardown
	// deadlock, as it did the first two times.
	go func() {
		for {
			if _, err := cl.nextStdout(); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}

	if !sawOnStream {
		t.Error("the event never reached the stream")
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "recorded") {
		t.Errorf("the event was not rendered into the task log:\n%s", joined)
	}
	if strings.Contains(joined, `"kind":"event"`) {
		t.Errorf("neutral JSON reached the log instead of a rendered line:\n%s", joined)
	}
}
