package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
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

	cmd := exec.CommandContext(runCtx, p.ClaudeBin, "-p", prompt)
	cmd.Dir = p.CWD
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
