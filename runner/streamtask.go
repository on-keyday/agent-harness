package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/trsf"
)

// StreamTask runs an event-stream agent for one task.
//
// The transport is agentexec with ptyEnabled=false, NOT a hand-rolled framer.
// The first version of this file said agentexec "exists to splice a PTY, and
// there is no PTY here" and framed NDJSON onto the stream itself. That was
// written without reading agentexec: ptyEnabled is a PARAMETER, and its false
// branch is a plain exec.CommandContext whose stdout/stderr are wrapped into
// FrameType_Stdout/Stderr and whose stdin is fed from FrameType_Stdin frames —
// which is what the hand-rolled version did, except in a framing that neither
// SessionMux nor the client's CommandExecutionStream speaks. The neutral NDJSON
// rides inside Stdout frames and needs no new frame type, which also keeps the
// standing rule that exec/frame is legacy and not extended.
//
// What this file still owns, and agentexec deliberately does not: the
// pending-approval table, the task-log rendering, and the adapter argv. Those
// hang off ExecuteOption.Audit, which taps raw stdout/stdin payloads — the hook
// that exists for exactly this and made a stream wrapper unnecessary.
//
// The three negatives from §2 of the design hold: this file does not parse
// vendor JSON (it reads the NEUTRAL protocol, which is the runner's own), does
// not know the permission sentinel, and does not decide approvals.
type StreamTask struct {
	// AdapterPath is the adapter binary. Resolved per task, not cached, so
	// editing an adapter reaches the next task with no rebuild and no restart.
	AdapterPath string
	// AgentArgv is the resolved agent command (bin plus any sandbox wrapper),
	// passed to the adapter after `--`. When the runner resolved --agent-bin to
	// agent-in-podman.sh this leaves the adapter outside the container and the
	// agent inside.
	AgentArgv []string
	Dir       string
	Env       []string
	Prompt    string

	ResumeConversation bool

	Logger *slog.Logger

	// LogSink receives rendered log lines in the same shape the oneshot path
	// publishes, so `logs` and the log panes need no changes for this kind.
	LogSink func([]byte)

	// OnPending fires when the pending-request count changes, so the task can
	// report `pending=N`. §3 puts this count on the task rather than adding a
	// TaskStatus value.
	OnPending func(n int)

	mu      sync.Mutex
	pending map[string]streamagent.Request
	exit    *streamagent.Exit
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

// Exit is what the adapter reported on its way out, or nil if it never did.
//
// It is the ONLY exit signal available for this kind: ExecuteCommandWithOption
// logs a failing errgroup and then returns nil unconditionally, so neither its
// return value nor Auditor.Exit carries a non-zero agent exit on the normal
// path. (handleOpenExec's `if runErr != nil` is dead for the same reason.) The
// adapter reporting its own exit in-band is what makes the code observable.
func (t *StreamTask) Exit() *streamagent.Exit {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit
}

// AdapterArgv is the command agentexec runs: the adapter, its own flags, then
// `--` and the agent.
func (t *StreamTask) AdapterArgv() []string {
	argv := []string{t.AdapterPath, "--dir", t.Dir}
	if t.Prompt != "" {
		argv = append(argv, "--prompt", t.Prompt)
	}
	if t.ResumeConversation {
		// An intent, not a vendor flag: the adapter picks the spelling.
		argv = append(argv, "--resume-conversation")
	}
	argv = append(argv, "--")
	return append(argv, t.AgentArgv...)
}

func (t *StreamTask) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

// Run carries the session until the agent exits or the stream ends. stream is
// the server-allocated bidi stream; agentexec closes it.
//
// Note what does NOT end this: a client detaching. The server holds the runner
// stream for the task's whole life (server/session_mux.go), so an event-stream
// task runs until it is cancelled, or until a client closes the agent's stdin
// with a ZERO-LENGTH Stdin frame — the clean `finish`, which agentexec's
// handleInput already implements and which the design had assumed did not
// exist.
func (t *StreamTask) Run(ctx context.Context, stream trsf.BidirectionalStream) error {
	if t.AdapterPath == "" {
		return fmt.Errorf("no stream adapter configured for this profile")
	}
	// Fail at start, loudly, rather than warn: §2's failure discipline. A warn
	// would hand the task a silently agent-less run, which is the shape the
	// vendor already has when its own flags are wrong.
	if _, err := exec.LookPath(t.AdapterPath); err != nil {
		return fmt.Errorf("stream adapter %q: %w", t.AdapterPath, err)
	}

	t.mu.Lock()
	t.pending = map[string]streamagent.Request{}
	t.exit = nil
	t.mu.Unlock()

	argv := t.AdapterArgv()
	return agentexec.ExecuteCommandWithOption(ctx, stream, t.logger(),
		argv[0], argv[1:], t.Dir,
		false, // no PTY: this kind is events, not a terminal
		t.Env,
		agentexec.ExecuteOption{Audit: &streamTap{task: t}})
}

func (t *StreamTask) log(s string) {
	if t.LogSink != nil {
		t.LogSink([]byte(s + "\n"))
	}
}

func (t *StreamTask) notifyPending(n int) {
	if t.OnPending != nil {
		t.OnPending(n)
	}
}

// streamTap reads the neutral protocol as it passes, without being in its way.
// It is an agentexec.Auditor: Stdout carries what the adapter wrote before it
// was framed, Stdin what a client sent after it was unframed.
//
// Two constraints come from that interface and are not optional. The buffers
// are REUSED by the caller, so anything retained past the call is copied; and
// the methods are called concurrently from the stdout, stderr and stdin
// goroutines, so each direction keeps its own line buffer under its own lock.
type streamTap struct {
	task *StreamTask

	outMu  sync.Mutex
	outBuf []byte

	inMu  sync.Mutex
	inBuf []byte
}

func (s *streamTap) Start(string, []string, bool) {}
func (s *streamTap) Exit(error)                   {}

func (s *streamTap) Stdout(data []byte) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.outBuf = appendLines(s.outBuf, data, s.onAdapterLine)
}

func (s *streamTap) Stdin(data []byte) {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	s.inBuf = appendLines(s.inBuf, data, s.onClientLine)
}

// Stderr is the AGENT's stderr, already passed through verbatim by the adapter.
// It reaches the client as Stderr frames on its own; this copy goes to the task
// log, tagged the way the oneshot path tags it.
func (s *streamTap) Stderr(data []byte) {
	for _, line := range splitLines(data) {
		s.task.log("[err]" + line)
	}
}

// onAdapterLine is the adapter → client direction.
func (s *streamTap) onAdapterLine(line []byte) {
	m, err := streamagent.DecodeMsg(line)
	if err != nil {
		// A line the runner cannot read still reaches the client untouched —
		// it was already framed by the time this ran. Logging it is all the
		// runner can honestly do; failing the task on a line it merely does not
		// understand would make an adapter newer than the runner fatal.
		s.task.log("[err]adapter line not understood: " + err.Error())
		return
	}
	switch m.Kind {
	case streamagent.KindRequest:
		if m.Request == nil {
			return
		}
		req := *m.Request
		s.task.mu.Lock()
		s.task.pending[req.ID] = req
		n := len(s.task.pending)
		s.task.mu.Unlock()
		s.task.notifyPending(n)
		s.task.log(fmt.Sprintf("[out]⏸ approval needed: %s (%s)", req.Tool, req.ID))
	case streamagent.KindEvent:
		if m.Event != nil {
			s.task.log("[out]" + agentlog.Render(toAgentlog(*m.Event)))
		}
	case streamagent.KindExit:
		if m.Exit != nil {
			ex := *m.Exit
			s.task.mu.Lock()
			s.task.exit = &ex
			s.task.mu.Unlock()
			if ex.Err != "" {
				s.task.log("[err]adapter: " + ex.Err)
			}
		}
	}
}

// onClientLine is the client → adapter direction.
func (s *streamTap) onClientLine(line []byte) {
	m, err := streamagent.DecodeMsg(line)
	if err != nil {
		return // a client's malformed line is the adapter's to reject
	}
	if m.Kind != streamagent.KindResponse || m.Response == nil {
		return
	}
	s.task.mu.Lock()
	_, known := s.task.pending[m.Response.ID]
	delete(s.task.pending, m.Response.ID)
	n := len(s.task.pending)
	s.task.mu.Unlock()
	if !known {
		return
	}
	s.task.notifyPending(n)
	// NOTE: this fires when the answer was FORWARDED, not when the agent acted
	// on it. There is no delivery ack in the protocol; see the wiring log.
	s.task.log(fmt.Sprintf("[out]▶ %s: %s", m.Response.ID, m.Response.Behavior))
}

// appendLines accumulates data into buf and calls fn for each complete line,
// returning the unconsumed remainder. Every line handed to fn is a fresh copy,
// because the Auditor contract says the caller's buffers are reused.
func appendLines(buf, data []byte, fn func([]byte)) []byte {
	buf = append(buf, data...)
	for {
		i := indexNewline(buf)
		if i < 0 {
			return buf
		}
		line := make([]byte, i)
		copy(line, buf[:i])
		buf = buf[i+1:]
		if len(line) > 0 {
			fn(line)
		}
	}
}

func indexNewline(b []byte) int {
	for i := range b {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}

// splitLines is appendLines' one-shot form for stderr, which needs no
// cross-call buffering: a partial trailing line is logged as-is rather than
// held back, because a diagnostic that arrives late is worse than one that
// arrives split.
func splitLines(data []byte) []string {
	var out []string
	start := 0
	for i := range data {
		if data[i] == '\n' {
			if i > start {
				out = append(out, string(data[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, string(data[start:]))
	}
	return out
}

// toAgentlog is the adapter's toNeutral inverted, so a neutral event renders
// through the SAME Render the oneshot path uses and the two kinds' log lines
// cannot drift. The round trip is asserted in the tests: if it were lossy, this
// kind's logs would quietly say less.
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
