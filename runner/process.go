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
)

// LogSink receives log chunks (each with a stream prefix already applied: "[out]" or "[err]").
type LogSink func(data []byte)

// Process wraps a single execution of the claude binary in a worktree.
type Process struct {
	ClaudeBin string        // path to the claude executable (or fake-claude.sh in tests)
	CWD       string        // worktree directory; cmd.Dir = CWD
	Timeout   time.Duration // max wall time; if zero, defaults to 30 minutes
	ExtraArgs []string      // additional args inserted before "-p <prompt>" (e.g. --dangerously-skip-permissions)
	Env       []string      // additional env vars to merge with os.Environ()

	// OnStdinWriter, if non-nil, is called once after the process stdin pipe
	// is ready. The argument is a write fn that can be used to inject bytes
	// into stdin from any goroutine while the process is running. Used by
	// Session.WakeStdin to deliver agentboard wake markers to non-interactive
	// (oneshot) tasks.
	OnStdinWriter func(write func([]byte) (int, error))
}

// Run starts ClaudeBin with `-p <prompt>`, captures stdout and stderr line-by-line,
// passes each line (with [out]/[err] prefix and trailing newline preserved) to sink,
// and returns the process exit code. The exit code is -1 if the process could not be started
// or was killed by signal/timeout.
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

	args := append([]string{}, p.ExtraArgs...)
	args = append(args, "-p", prompt)
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

	// Wire up a writable stdin pipe when the caller wants to inject wake
	// markers. If OnStdinWriter is nil, cmd.Stdin stays nil (reads from
	// /dev/null-equivalent).
	//
	// Lifecycle constraint: the exec-internal stdin-copy goroutine blocks on
	// stdinPipeR.Read; cmd.Wait waits for that goroutine. To avoid a deadlock
	// we must close stdinPipeW BEFORE cmd.Wait can return.
	//
	// We solve this with a procDone channel that is closed by a dedicated
	// watcher goroutine (which calls cmd.Process.Wait) immediately after the
	// OS-level process exits. The stdin-closer goroutine listens on procDone
	// and closes stdinPipeW — this unblocks the exec-internal goroutine so
	// cmd.Wait can finish.
	//
	// Calling cmd.Process.Wait in the watcher is safe: on Linux the result is
	// cached in os.Process after the first waitpid syscall, so the subsequent
	// cmd.Wait call reads the cached exit status instead of issuing a second
	// waitpid.
	var stdinPipeW *io.PipeWriter
	if p.OnStdinWriter != nil {
		var stdinPipeR *io.PipeReader
		stdinPipeR, stdinPipeW = io.Pipe()
		cmd.Stdin = stdinPipeR
	}

	if err := cmd.Start(); err != nil {
		if stdinPipeW != nil {
			stdinPipeW.Close()
		}
		return -1, fmt.Errorf("start: %w", err)
	}

	if p.OnStdinWriter != nil {
		writeFn := func(b []byte) (int, error) {
			return stdinPipeW.Write(b)
		}
		p.OnStdinWriter(writeFn)

		// procDone is closed once the OS process has exited.
		procDone := make(chan struct{})
		proc := cmd.Process
		go func() {
			proc.Wait() //nolint:errcheck // only used to detect exit
			close(procDone)
		}()
		go func() {
			select {
			case <-runCtx.Done():
			case <-procDone:
			}
			stdinPipeW.Close()
		}()
	}

	var wg sync.WaitGroup
	scan := func(r io.Reader, prefix []byte) {
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
	wg.Add(2)
	go scan(stdout, []byte("[out]"))
	go scan(stderr, []byte("[err]"))

	waitErr := cmd.Wait()
	wg.Wait()

	exit := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
			if exit == -1 {
				// killed by signal (e.g., SIGKILL after timeout)
				exit = -1
			}
		} else {
			exit = -1
		}
	}
	return exit, nil
}
