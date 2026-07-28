package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// LogSink receives log chunks (each with a stream prefix already applied: "[out]" or "[err]").
type LogSink func(data []byte)

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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}

	var wg sync.WaitGroup
	// emit publishes one already-rendered line under the given stream prefix.
	emit := func(prefix, text string) {
		buf := make([]byte, 0, len(prefix)+len(text)+1)
		buf = append(buf, prefix...)
		buf = append(buf, text...)
		buf = append(buf, '\n')
		sink(buf)
	}
	// scanRaw forwards each line verbatim, preserving its original newline.
	// Used for stderr, where decoding would suppress crash output.
	scanRaw := func(r io.Reader, prefix []byte) {
		defer wg.Done()
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				buf := make([]byte, 0, len(prefix)+len(line))
				buf = append(buf, prefix...)
				buf = append(buf, line...)
				sink(buf)
			}
			if err != nil {
				return
			}
		}
	}
	// scanDecoded runs each stdout line through the profile's decoder and
	// publishes one log line per resulting event. A final partial line (no
	// trailing newline before EOF) is decoded too, so nothing is lost at exit.
	// Only used when p.LogFormat names a real decoder (see below) — the
	// decode/render round-trip is not byte-preserving (passthrough.Decode
	// trims "\r\n" and emit always adds back exactly one "\n"), so it must
	// never run for the "nothing to decode" case.
	scanDecoded := func(r io.Reader) {
		defer wg.Done()
		dec := agentlog.NewDecoder(p.LogFormat)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				for _, ev := range dec.Decode(line) {
					emit("[out]", agentlog.Render(ev))
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	if agentlog.HasDecoder(p.LogFormat) {
		go scanDecoded(stdout)
	} else {
		// Empty or unrecognised LogFormat: forward stdout exactly like
		// stderr, byte-for-byte, so a CRLF line or an unterminated final
		// line reaches sink unchanged instead of going through
		// passthrough's lossy Decode/Render round-trip.
		go scanRaw(stdout, []byte("[out]"))
	}
	go scanRaw(stderr, []byte("[err]"))

	waitErr := cmd.Wait()
	wg.Wait()

	exit := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
			// exit == -1 means killed by signal (e.g., SIGKILL after timeout)
		} else {
			exit = -1
		}
	}
	return exit, nil
}
