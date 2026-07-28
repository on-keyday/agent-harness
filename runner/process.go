package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// LogSink receives log chunks (each with a stream prefix already applied: "[out]" or "[err]").
type LogSink func(data []byte)

// emitLine publishes one already-rendered line under the given stream
// prefix, adding exactly one trailing newline. Used for decoded events,
// whose rendered text never carries its own terminator.
func emitLine(sink LogSink, prefix, text string) {
	buf := make([]byte, 0, len(prefix)+len(text)+1)
	buf = append(buf, prefix...)
	buf = append(buf, text...)
	buf = append(buf, '\n')
	sink(buf)
}

// rawLineWriter is an io.Writer assigned to cmd.Stdout/cmd.Stderr. It
// forwards each complete line verbatim — original terminator included — to
// sink, prefixed by prefix. Used for stderr (always) and for stdout when no
// agentlog decoder applies to p.LogFormat.
//
// Because os/exec creates the underlying pipe itself when Stdout/Stderr are
// not *os.File and copies into this Writer in its own goroutine, Write here
// receives arbitrary byte chunks, not lines — so it buffers and splits on
// '\n' itself. Nothing calls Write again once the child's fd closes, so a
// buffered partial line (a final line with no trailing '\n') would sit here
// forever unless flush is called explicitly after cmd.Wait() returns.
//
// Not safe for concurrent Write calls: os/exec's internal copy goroutine is
// the sole caller for a given stream, so none is needed.
type rawLineWriter struct {
	prefix []byte
	sink   LogSink
	buf    []byte
}

func (w *rawLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *rawLineWriter) emit(line []byte) {
	buf := make([]byte, 0, len(w.prefix)+len(line))
	buf = append(buf, w.prefix...)
	buf = append(buf, line...)
	w.sink(buf)
}

// flush delivers a trailing partial line (no terminator) left buffered when
// the child exits without a final '\n'. Must be called once, after
// cmd.Wait() returns. No terminator is synthesized: the raw path forwards
// stdout/stderr byte-for-byte, including the absence of a final newline.
func (w *rawLineWriter) flush() {
	if len(w.buf) == 0 {
		return
	}
	w.emit(w.buf)
	w.buf = nil
}

// decodedLineWriter is an io.Writer assigned to cmd.Stdout when
// agentlog.HasDecoder(LogFormat) is true. It runs each complete stdout line
// through dec and publishes one rendered "[out]" line per resulting event —
// the decoded counterpart of rawLineWriter, with the same buffer-and-split
// and explicit-flush requirements documented there.
type decodedLineWriter struct {
	dec  agentlog.Decoder
	sink LogSink
	buf  []byte
}

func (w *decodedLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.decode(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *decodedLineWriter) decode(line []byte) {
	for _, ev := range w.dec.Decode(line) {
		emitLine(w.sink, "[out]", agentlog.Render(ev))
	}
}

// flush decodes and delivers a trailing partial line (no terminator) left
// buffered when the child exits without a final '\n'. Must be called once,
// after cmd.Wait() returns — this is what makes a final unterminated event
// (typically the agent's "result"/finish line) reach sink instead of being
// silently dropped; see TestProcessDecodesFinalUnterminatedLine.
func (w *decodedLineWriter) flush() {
	if len(w.buf) == 0 {
		return
	}
	w.decode(w.buf)
	w.buf = nil
}

// Process wraps a single execution of the claude binary in a worktree.
type Process struct {
	ClaudeBin                 string        // path to the claude executable (or fake-claude.sh in tests)
	CWD                       string        // worktree directory; cmd.Dir = CWD
	Timeout                   time.Duration // max wall time; if zero, defaults to 30 minutes
	ExtraArgs                 []string      // runner-global args plus per-task args
	ResumeConversation        bool          // when true, ask the agent CLI to resume its prior conversation
	OneshotArgvTemplate       []string      // argv template for oneshot mode; defaults to "{args} -p {prompt}"
	ResumeOneshotArgvTemplate []string      // argv template for resume-conversation oneshot mode
	Env                       []string      // additional env vars to merge with os.Environ()

	// LogFormat selects the agentlog decoder applied to stdout. Empty means
	// raw passthrough. stderr is never decoded.
	LogFormat string
}

// Run starts ClaudeBin with `-p <prompt>`, captures stdout and stderr line-by-line,
// and returns the process exit code. stderr is always forwarded to sink verbatim
// (with an "[err]" prefix, original line bytes and terminator untouched). stdout is
// forwarded the same way — verbatim, "[out]"-prefixed — when LogFormat is empty or
// names a format agentlog does not recognise; when it names a real decoder, each
// stdout line is decoded and every resulting event is rendered and sent to sink as
// its own "[out]"-prefixed, newline-terminated line instead of the raw JSON. The
// exit code is -1 if the process could not be started or was killed by signal/timeout.
//
// Run blocks until the process exits or ctx is cancelled. On ctx cancellation or timeout,
// the process is sent SIGTERM and given 5 seconds before SIGKILL.
func (p *Process) Run(ctx context.Context, prompt string, sink LogSink) (int, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args, err := buildOneshotArgs(p.OneshotArgvTemplate, p.ResumeOneshotArgvTemplate, p.ExtraArgs, prompt, p.ResumeConversation)
	if err != nil {
		return -1, err
	}
	cmd := exec.CommandContext(runCtx, p.ClaudeBin, args...)
	cmd.Dir = p.CWD
	if len(p.Env) > 0 {
		cmd.Env = append(os.Environ(), p.Env...)
	}
	// Give SIGTERM 5s grace before SIGKILL when ctx fires.
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}

	// cmd.Stdout/cmd.Stderr are writers, not pipes taken via
	// cmd.StdoutPipe()/cmd.StderrPipe(). Following
	// github.com/on-keyday/objtrsf/exec (the package the interactive path
	// already uses) rather than swapping the wg.Wait()/cmd.Wait() order:
	// when Stdout/Stderr are not *os.File, os/exec creates the pipe itself,
	// copies into the Writer in its own goroutine, and cmd.Wait() waits for
	// that copy to finish before returning. There is no window in which
	// cmd.Wait() can close a pipe out from under a still-draining reader,
	// so no caller-side sync.WaitGroup is needed either.
	stderrW := &rawLineWriter{prefix: []byte("[err]"), sink: sink}
	cmd.Stderr = stderrW

	var stdoutFlush func()
	if agentlog.HasDecoder(p.LogFormat) {
		stdoutW := &decodedLineWriter{dec: agentlog.NewDecoder(p.LogFormat), sink: sink}
		cmd.Stdout = stdoutW
		stdoutFlush = stdoutW.flush
	} else {
		// Empty or unrecognised LogFormat: forward stdout exactly like
		// stderr, byte-for-byte, so a CRLF line or an unterminated final
		// line reaches sink unchanged instead of going through
		// passthrough's lossy Decode/Render round-trip.
		stdoutW := &rawLineWriter{prefix: []byte("[out]"), sink: sink}
		cmd.Stdout = stdoutW
		stdoutFlush = stdoutW.flush
	}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}

	waitErr := cmd.Wait()
	// os/exec stops calling Write once the child's fd closes; nothing signals
	// EOF to a Writer. Any line buffered without a trailing '\n' — the
	// child's last write, if it never emitted one — must be flushed
	// explicitly here or it is silently lost. See TestProcessDecodesFinalUnterminatedLine
	// and TestProcessStdoutVerbatimWhenNoDecoderApplies.
	stdoutFlush()
	stderrW.flush()

	exit := 0
	if waitErr != nil {
		if errors.Is(waitErr, exec.ErrWaitDelay) {
			// The child exited on its own (cmd.ProcessState reflects its real
			// exit, captured by cmd.Process.Wait() before this error could ever
			// occur — see (*exec.Cmd).Wait); ErrWaitDelay only means some
			// descendant kept stdout/stderr open past that and os/exec had to
			// force-close the pipes after cmd.WaitDelay. That's not a failure
			// of the agent itself, so classify by the child's real exit code,
			// not by this I/O-only error.
			exit = cmd.ProcessState.ExitCode()
		} else if ee, ok := waitErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
			// exit == -1 means killed by signal (e.g., SIGKILL after timeout)
		} else {
			exit = -1
		}
	}
	return exit, nil
}
