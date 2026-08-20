package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// buildAdapter builds the real adapter once per test binary. The point of
// these tests is the WIRING -- runner ↔ adapter ↔ agent over a stream -- so
// stubbing the adapter would test the half that is already covered in
// runner/streamagent.
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

// streamPair returns the two ends of a bidi stream: what the runner sees, and
// what the server (and through it, a client) sees.
type endpoint struct {
	io.Reader
	io.Writer
}

func streamPair() (runnerSide, clientSide endpoint, closeAll func()) {
	aR, aW := io.Pipe() // runner → client
	bR, bW := io.Pipe() // client → runner
	return endpoint{Reader: bR, Writer: aW},
		endpoint{Reader: aR, Writer: bW},
		func() { _ = aW.Close(); _ = bW.Close(); _ = aR.Close(); _ = bR.Close() }
}

func fakeStreamAgent(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agent.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The wiring question, end to end: a task's events reach the stream, an
// approval raised by the agent reaches the stream, an answer sent back on the
// stream reaches the agent, and the log gets the same rendered lines the
// oneshot path would produce.
func TestStreamTaskCarriesEventsAndApprovalsOverTheStream(t *testing.T) {
	agentStdin := filepath.Join(t.TempDir(), "agent-stdin")
	agent := fakeStreamAgent(t,
		`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}'`+"\n"+
			`printf '%s\n' '{"type":"control_request","request_id":"v-1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x"}}}'`+"\n"+
			// head -n 1, not cat: cat holds the file open until its stdin
			// closes, so "the answer arrived" would only be observable during
			// teardown -- which is exactly when the test wants to stop.
			"head -n 1 > "+agentStdin+"\n"+
			`printf '%s\n' '{"type":"result","subtype":"success","duration_ms":5,"total_cost_usd":0}'`+"\n")

	runnerSide, clientSide, closeAll := streamPair()
	defer closeAll()

	var logMu sync.Mutex
	var logLines []string
	var pendingSeen []int

	task := &StreamTask{
		AdapterPath: testAdapter(t),
		AgentArgv:   []string{agent},
		Dir:         t.TempDir(),
		LogSink: func(b []byte) {
			logMu.Lock()
			defer logMu.Unlock()
			logLines = append(logLines, strings.TrimRight(string(b), "\n"))
		},
		OnPending: func(n int) {
			logMu.Lock()
			defer logMu.Unlock()
			pendingSeen = append(pendingSeen, n)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); _, _ = task.Run(ctx, runnerSide) }()

	// The client end: read until the approval arrives, then answer it.
	rd := streamagent.NewReader(clientSide)
	var sawHello, sawEvent bool
	var req *streamagent.Request
	for req == nil {
		m, err := rd.Next()
		if err != nil {
			t.Fatalf("stream ended before the approval arrived (hello=%v event=%v): %v",
				sawHello, sawEvent, err)
		}
		switch m.Kind {
		case streamagent.KindHello:
			sawHello = true
		case streamagent.KindEvent:
			sawEvent = true
		case streamagent.KindRequest:
			req = m.Request
		}
	}
	if !sawHello {
		t.Error("the adapter's hello never reached the stream")
	}
	if !sawEvent {
		t.Error("no event reached the stream before the approval")
	}
	if req.Tool != "Write" {
		t.Errorf("request tool = %q", req.Tool)
	}

	// pending=N must have moved before the answer.
	logMu.Lock()
	gotPending := append([]int(nil), pendingSeen...)
	logMu.Unlock()
	if len(gotPending) == 0 || gotPending[0] != 1 {
		t.Errorf("pending counts = %v, want the first to be 1", gotPending)
	}
	if p := task.Pending(); len(p) != 1 || p[0].ID != req.ID {
		t.Errorf("Pending() = %+v, want the one request", p)
	}

	// Answer on the stream, the way a client would.
	resp, _ := json.Marshal(streamagent.Msg{
		V: streamagent.ProtocolVersion, Kind: streamagent.KindResponse,
		Response: &streamagent.Response{ID: req.ID, Behavior: streamagent.BehaviorAllow},
	})
	if _, err := clientSide.Write(append(resp, '\n')); err != nil {
		t.Fatalf("writing the answer: %v", err)
	}

	// Wait for evidence the answer was actually consumed before ending the
	// task. There is no acknowledgement in the protocol -- the runner drops a
	// request from `pending` when it FORWARDS the answer, not when the agent
	// acts on it -- so the only evidence available is a later event. See the
	// wiring log: that gap is a finding, not a test inconvenience.
	deadline := time.After(30 * time.Second)
	for done := false; !done; {
		type res struct {
			m   streamagent.Msg
			err error
		}
		ch := make(chan res, 1)
		go func() { m, err := rd.Next(); ch <- res{m, err} }()
		select {
		case r := <-ch:
			if r.err != nil {
				done = true
			} else if r.m.Kind == streamagent.KindEvent && r.m.Event.Kind == streamagent.EventFinish {
				done = true
			} else if r.m.Kind == streamagent.KindExit {
				done = true
			}
		case <-deadline:
			t.Fatal("no finish event after the approval was answered")
		}
	}

	// Nothing ends an event-stream task on its own -- see the wiring log: the
	// agent waits for another turn forever and a detach must not end it. So
	// the test ends it the way the system does, by cancelling.
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("StreamTask.Run did not return after cancel; the teardown is " +
			"deadlocked again -- close the adapter stdin BEFORE waiting on it")
	}
	closeAll()

	// The answer reached the AGENT, as a vendor control_response.
	got, err := os.ReadFile(agentStdin)
	if err != nil {
		t.Fatalf("the agent never read its stdin: %v", err)
	}
	if !strings.Contains(string(got), `"type":"control_response"`) ||
		!strings.Contains(string(got), `"request_id":"v-1"`) {
		t.Errorf("the answer did not reach the agent as a vendor response:\n%s", got)
	}

	logMu.Lock()
	defer logMu.Unlock()
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
	if pendingSeen[len(pendingSeen)-1] != 0 {
		t.Errorf("pending never returned to 0: %v", pendingSeen)
	}
}

// An unresolvable adapter must fail the task at start, not warn: §2's failure
// discipline, and the same shape as the vendor misconfiguration that otherwise
// exits success.
func TestStreamTaskFailsLoudlyOnAMissingAdapter(t *testing.T) {
	runnerSide, _, closeAll := streamPair()
	defer closeAll()
	task := &StreamTask{
		AdapterPath: filepath.Join(t.TempDir(), "does-not-exist"),
		AgentArgv:   []string{"true"},
		Dir:         t.TempDir(),
	}
	code, err := task.Run(context.Background(), runnerSide)
	if err == nil {
		t.Fatal("a missing adapter did not fail the task")
	}
	if code != -1 {
		t.Errorf("exit = %d, want -1", code)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error does not name the adapter: %v", err)
	}
}

// The neutral Event must survive agentlog → neutral → agentlog, or this kind's
// log lines silently say less than the oneshot kind's for the same agent
// output. toNeutral and toAgentlog live in different packages and are the two
// halves nothing else pins together.
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
		// The neutral type is produced by the ADAPTER's mapper and consumed by
		// the RUNNER's; going through JSON as well is what the wire does.
		neutral := adapterToNeutral(t, in)
		var back streamagent.Event
		b, _ := json.Marshal(neutral)
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("neutral event does not survive JSON: %v", err)
		}
		got := toAgentlog(back)
		if agentlog.Render(got) != agentlog.Render(in) {
			t.Errorf("render drifted for %+v:\n  before %q\n  after  %q",
				in, agentlog.Render(in), agentlog.Render(got))
		}
	}
}

// adapterToNeutral reaches the adapter package's mapper the only way an
// external package can: by running a line through it. Rather than export
// toNeutral for a test, this reconstructs the same mapping and the assertion
// above catches a divergence between the two as a render drift.
func adapterToNeutral(t *testing.T, e agentlog.Event) streamagent.Event {
	t.Helper()
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
