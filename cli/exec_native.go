//go:build !js

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ExecOptions configures SessionExec.
type ExecOptions struct {
	Timeout time.Duration // max wait for completion; <=0 uses execDefaultTimeout
	Raw     bool          // return verbatim output bytes (skip interpretPlain)
}

// ExecResult is the outcome of a SessionExec.
type ExecResult struct {
	ExitCode int           // command exit code; -1 when TimedOut / unknown
	Output   []byte        // interpreted plain text (default) or verbatim bytes (Raw)
	TimedOut bool          // completion sentinel not seen before Timeout
	Duration time.Duration // wall time from injection to completion/timeout
}

const execDefaultTimeout = 30 * time.Second

// SessionExec runs a single shell command line synchronously inside a
// detachable interactive session's foreground shell and returns its combined
// (stdout+stderr) output plus exit code. It is a client-side orchestration of
// the cowrite attach (like SessionSend, it is a method on the long-lived
// *Client, so TUI/WebUI callers reuse their existing client): it injects
// `printf '<S>\n'; <cmd>; printf '<E>%s\n' "$?"` as one physical line, then
// reads the PTY output stream until the END sentinel line appears — the
// synchronous completion signal, matched against the whole accumulated buffer
// so a frame-split sentinel never completes early.
//
// Because the command travels through the foreground PTY, it runs in whatever
// shell context is live there — including a shell reached over ssh or inside a
// netns — which is exactly why a runner-side out-of-band exec cannot serve this
// use. The consequence is that stdout and stderr are not separable (one PTY
// stream); Output is their interleaving. The foreground must be a POSIX-ish
// shell (printf / $? work); otherwise no sentinel appears and the call times
// out (TimedOut=true, ExitCode=-1).
//
// cmd must be a single logical line; it may compose with ; && || | $().
func (c *Client) SessionExec(ctx context.Context, taskIDHex, cmd string, opts ExecOptions) (ExecResult, error) {
	if strings.ContainsAny(cmd, "\n\r") {
		return ExecResult{}, fmt.Errorf("session exec: multi-line command not supported; join with ';' or '&&' into one line")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = execDefaultTimeout
	}

	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return ExecResult{}, fmt.Errorf("session exec: nonce: %w", err)
	}
	nonce := hex.EncodeToString(nb[:])
	s := execSentinels{start: "__HEXEC_" + nonce + "_S__", end: "__HEXEC_" + nonce + "_E__"}

	stream, _, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_Cowrite)
	if err != nil {
		return ExecResult{}, err
	}
	defer stream.Close()

	// One physical line (submitted with a CR). The trailing printf runs as the
	// next element of the list, so "$?" is <cmd>'s exit status.
	inject := "printf '" + s.start + `\n'; ` + cmd + "; printf '" + s.end + `%s\n' "$?"` + "\r"
	if _, err := stream.Stdin().Write([]byte(inject)); err != nil {
		return ExecResult{}, fmt.Errorf("session exec: inject: %w", err)
	}

	start := time.Now()
	var mu sync.Mutex
	var acc []byte
	resultCh := make(chan execScan, 1)
	go func() {
		buf := make([]byte, 32*1024)
		out := stream.Stdout()
		for {
			n, rerr := out.Read(buf)
			if n > 0 {
				mu.Lock()
				acc = append(acc, buf[:n]...)
				snap := append([]byte(nil), acc...)
				mu.Unlock()
				if r := scanExec(snap, s); r.done {
					resultCh <- r
					return
				}
			}
			if rerr != nil {
				mu.Lock()
				snap := append([]byte(nil), acc...)
				mu.Unlock()
				resultCh <- scanExec(snap, s) // may be done=false (stream closed early)
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-resultCh:
		if r.done {
			return buildExecResult(r, opts, time.Since(start)), nil
		}
		return partialExecResult(&mu, &acc, s, opts, time.Since(start)), nil
	case <-timer.C:
		return partialExecResult(&mu, &acc, s, opts, time.Since(start)), nil
	case <-ctx.Done():
		return partialExecResult(&mu, &acc, s, opts, time.Since(start)), ctx.Err()
	}
}

// buildExecResult wraps a completed scan into an ExecResult, interpreting the
// output to plain text unless Raw was requested.
func buildExecResult(r execScan, opts ExecOptions, dur time.Duration) ExecResult {
	out := r.output
	if !opts.Raw {
		out = []byte(interpretPlain(r.output))
	}
	return ExecResult{ExitCode: r.exitCode, Output: out, TimedOut: false, Duration: dur}
}

// partialExecResult is used when the END sentinel never arrived (timeout or an
// early stream close). It re-scans (in case the sentinel landed in the final
// bytes) and otherwise returns best-effort output after the START line, marked
// TimedOut with an unknown exit code.
func partialExecResult(mu *sync.Mutex, acc *[]byte, s execSentinels, opts ExecOptions, dur time.Duration) ExecResult {
	mu.Lock()
	snap := append([]byte(nil), (*acc)...)
	mu.Unlock()

	if r := scanExec(snap, s); r.done {
		return buildExecResult(r, opts, dur)
	}
	part := partialOutput(snap, s)
	out := part
	if !opts.Raw {
		out = []byte(interpretPlain(part))
	}
	return ExecResult{ExitCode: -1, Output: out, TimedOut: true, Duration: dur}
}
