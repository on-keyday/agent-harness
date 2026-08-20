package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// StreamTask runs an event-stream agent for one task: it spawns the adapter,
// carries neutral NDJSON between the adapter and the server-allocated stream,
// and keeps the pending-approval table.
//
// It deliberately does NOT use agentexec. That package exists to splice a PTY,
// and there is no PTY here — the precedent this follows is
// handleOpenFileTransfer, which waits for the server-allocated stream and then
// speaks its own protocol on it.
//
// The three negatives from §2 of the design hold here and are worth stating
// where they can be checked: this file does not parse vendor JSON (it reads the
// NEUTRAL protocol, which is the runner's own), does not know the permission
// sentinel, and does not decide approvals — it routes them and blocks.
type StreamTask struct {
	// AdapterPath is the adapter binary. Read per task, not cached, so editing
	// an adapter reaches the next task with no rebuild and no restart.
	AdapterPath string
	// AgentArgv is the resolved agent command (bin + any sandbox wrapper),
	// passed to the adapter after `--`.
	AgentArgv []string
	Dir       string
	Env       []string
	Prompt    string

	ResumeConversation bool

	// LogSink receives rendered log lines in the same shape the oneshot path
	// publishes, so `logs` and the log panes need no changes for this kind.
	LogSink func([]byte)

	// OnPending fires when the pending-request count changes, so the task can
	// report `pending=N`. §3 puts this count on the task rather than adding a
	// TaskStatus value.
	OnPending func(n int)

	mu      sync.Mutex
	pending map[string]streamagent.Request
}

// Pending returns the requests currently awaiting an answer.
func (t *StreamTask) Pending() []streamagent.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]streamagent.Request, 0, len(t.pending))
	for _, r := range t.pending {
		out = append(out, r)
	}
	return out
}

// Run carries the session until the adapter exits or the stream closes.
// stream is the server-allocated bidi stream: neutral NDJSON both ways.
func (t *StreamTask) Run(ctx context.Context, stream io.ReadWriter) (int, error) {
	if t.AdapterPath == "" {
		return -1, fmt.Errorf("no stream adapter configured for this profile")
	}
	// Fail at start, loudly, rather than warn: §2's failure discipline. An
	// unresolvable adapter with a warning would hand the task a silently
	// agent-less run.
	if _, err := exec.LookPath(t.AdapterPath); err != nil {
		return -1, fmt.Errorf("stream adapter %q: %w", t.AdapterPath, err)
	}

	argv := []string{t.AdapterPath, "--dir", t.Dir}
	if t.Prompt != "" {
		argv = append(argv, "--prompt", t.Prompt)
	}
	if t.ResumeConversation {
		argv = append(argv, "--resume-conversation")
	}
	argv = append(argv, "--")
	argv = append(argv, t.AgentArgv...)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(), t.Env...)
	adapterIn, err := cmd.StdinPipe()
	if err != nil {
		return -1, err
	}
	adapterOut, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	adapterErr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start adapter: %w", err)
	}

	t.mu.Lock()
	t.pending = map[string]streamagent.Request{}
	t.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	// adapter → stream, with a tap for the log and the pending table.
	go func() { defer wg.Done(); t.pumpOut(adapterOut, stream) }()
	// The adapter's stderr is the AGENT's stderr, already passed through
	// verbatim. It goes to the task log tagged, exactly as the oneshot path
	// tags it, and never onto the neutral stream.
	go func() { defer wg.Done(); t.pumpStderr(adapterErr) }()

	// stream → adapter. Closing the adapter's stdin when this returns is what
	// STARTS the teardown, and the ordering is load-bearing: an event-stream
	// agent waits for another turn forever, so waiting for the process before
	// closing its input deadlocks — the runner waits for the adapter, the
	// adapter waits for its stdin, the agent waits for its own. Measured, as a
	// 300-second test hang, before it was written this way.
	// Not waited on. A plain io.Reader has no cancellable read, so this
	// goroutine ends only when the STREAM ends -- which in the real system is
	// the server closing it as the task finishes, and never on our own
	// timeline. Waiting for it here deadlocks a cancelled task: the second
	// hang measured while writing this, after the first was fixed.
	go func() {
		t.pumpIn(stream, adapterIn)
		_ = adapterIn.Close()
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitCh:
		// The agent finished on its own (a one-shot-shaped prompt, or a crash).
	case <-ctx.Done():
		// The task was cancelled. This is the ORDINARY way an event-stream
		// task ends: nothing else can end it, because a detach must not and
		// the agent will not. See the wiring log for why that is a semantic
		// this kind does not share with the other two.
		_ = adapterIn.Close()
		waitErr = <-waitCh
	}
	wg.Wait()
	return exitStatus(waitErr), waitErr
}

func (t *StreamTask) pumpOut(adapterOut io.Reader, stream io.Writer) {
	rd := streamagent.NewReader(adapterOut)
	for {
		m, err := rd.Next()
		if err != nil {
			return
		}
		switch m.Kind {
		case streamagent.KindRequest:
			if m.Request != nil {
				t.mu.Lock()
				t.pending[m.Request.ID] = *m.Request
				n := len(t.pending)
				t.mu.Unlock()
				t.notifyPending(n)
				t.log("[out]" + fmt.Sprintf("⏸ approval needed: %s (%s)", m.Request.Tool, m.Request.ID))
			}
		case streamagent.KindEvent:
			if m.Event != nil {
				t.log("[out]" + agentlog.Render(toAgentlog(*m.Event)))
			}
		case streamagent.KindExit:
			if m.Exit != nil && m.Exit.Err != "" {
				t.log("[err]adapter: " + m.Exit.Err)
			}
		}
		// Everything is forwarded verbatim, including kinds this build does
		// not act on: the runner is a router, and a client that understands a
		// newer adapter must not be cut off by an older runner.
		if err := writeLine(stream, m); err != nil {
			return
		}
	}
}

func (t *StreamTask) pumpIn(stream io.Reader, adapterIn io.Writer) {
	rd := streamagent.NewReader(stream)
	for {
		m, err := rd.Next()
		if err != nil {
			return
		}
		if m.Kind == streamagent.KindResponse && m.Response != nil {
			t.mu.Lock()
			_, known := t.pending[m.Response.ID]
			delete(t.pending, m.Response.ID)
			n := len(t.pending)
			t.mu.Unlock()
			if known {
				t.notifyPending(n)
				t.log("[out]" + fmt.Sprintf("▶ %s: %s", m.Response.ID, m.Response.Behavior))
			}
		}
		if err := writeLine(adapterIn, m); err != nil {
			return
		}
	}
}

func (t *StreamTask) pumpStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		t.log("[err]" + sc.Text())
	}
}

func (t *StreamTask) notifyPending(n int) {
	if t.OnPending != nil {
		t.OnPending(n)
	}
}

func (t *StreamTask) log(s string) {
	if t.LogSink != nil {
		t.LogSink([]byte(s + "\n"))
	}
}

func writeLine(w io.Writer, m streamagent.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// toAgentlog is toNeutral's inverse, so a neutral event renders through the
// SAME Render the oneshot path uses and the two kinds' log lines cannot drift.
// The round trip agentlog.Event → neutral → agentlog.Event is asserted in the
// tests: if it were lossy, this kind's logs would quietly say less.
func toAgentlog(e streamagent.Event) agentlog.Event {
	out := agentlog.Event{
		Text: e.Text, Tool: e.Tool, Args: e.Args, Result: e.Result,
		ExitCode: e.ExitCode, IsError: e.IsError, Warning: e.Warning,
	}
	switch e.Kind {
	case streamagent.EventSessionStart:
		out.Kind = agentlog.KindSessionStart
	case streamagent.EventThinking:
		out.Kind = agentlog.KindThinking
	case streamagent.EventToolStart:
		out.Kind = agentlog.KindToolStart
	case streamagent.EventToolEnd:
		out.Kind = agentlog.KindToolEnd
	case streamagent.EventText:
		out.Kind = agentlog.KindText
	case streamagent.EventFinish:
		out.Kind = agentlog.KindFinish
	case streamagent.EventError:
		out.Kind = agentlog.KindError
	default:
		out.Kind = agentlog.KindRaw
	}
	if e.Stats != nil {
		out.Stats = agentlog.Stats{
			DurationMS: e.Stats.DurationMS, CostUSD: e.Stats.CostUSD,
			InputTokens: e.Stats.InputTokens, OutputTokens: e.Stats.OutputTokens,
		}
	}
	return out
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
