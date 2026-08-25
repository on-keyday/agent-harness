package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ExecRunOutputMsg is one line the command printed. Stderr distinguishes the
// two streams, which is the entire reason this verb exists rather than
// `session exec` — merging them in the display would throw the property away
// on the last hop.
type ExecRunOutputMsg struct {
	Line   string
	Stderr bool
}

// ExecRunDoneMsg reports how the command ended. Err is a transport or refusal
// failure (nothing ran); Result is what the command itself did.
type ExecRunDoneMsg struct {
	TaskID  string
	Argv    []string
	Result  cli.ExecRunResult
	Dropped uint64
	Err     error
}

// ExecRunListMsg carries the result of `exec ls`.
type ExecRunListMsg struct {
	Execs []protocol.ExecRunInfo
	Err   error
}

// ExecRunKillMsg carries the result of `exec kill <id>`.
type ExecRunKillMsg struct {
	ExecID uint64
	Err    error
}

// execLineWriter splits a stream into lines and hands each to send WITHOUT ever
// blocking the caller.
//
// The no-block rule is the same one forwardStatusLogf documents: program.Send
// writes to an unbuffered channel, so a direct Send from an output pump parks
// whenever the event loop is not draining — and a parked pump backpressures the
// stream, which stalls the child part-way through its output.
//
// Unlike a cosmetic forward status line, dropped output is a LIE about what the
// command printed, so overflow is counted and reported rather than swallowed.
type execLineWriter struct {
	buf     bytes.Buffer
	stderr  bool
	ch      chan<- ExecRunOutputMsg
	dropped *atomic.Uint64
}

func (w *execLineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No newline yet: put the partial back and wait for more.
			w.buf.Reset()
			w.buf.WriteString(line)
			return len(p), nil
		}
		w.emit(strings.TrimRight(line, "\r\n"))
	}
}

// flush emits whatever is left when the stream ends without a trailing newline.
// A command whose last line has no "\n" still printed that line.
func (w *execLineWriter) flush() {
	if w.buf.Len() > 0 {
		w.emit(w.buf.String())
		w.buf.Reset()
	}
}

func (w *execLineWriter) emit(line string) {
	select {
	case w.ch <- ExecRunOutputMsg{Line: line, Stderr: w.stderr}:
	default:
		w.dropped.Add(1)
	}
}

// execOutputLine renders one output line for cmdresult. stderr is marked, not
// merged: an operator reading the panel must be able to tell which stream a
// line came from without re-running the command.
//
// The line is SANITIZED, for the reason sanitizeOutput's own doc comment gives
// about the raw-forward pane: this is arbitrary output from a command nobody
// vetted, drawn inside a bordered panel, and one ESC sequence in it repositions
// the cursor before the frame is drawn over the result. `make test` on a
// colourised build and any progress bar with a bare CR are the ordinary cases,
// not hostile ones. The pane had only trusted producers until this verb.
func execOutputLine(line string, stderr bool) string {
	clean := sanitizeOutput([]byte(line))
	if stderr {
		return WarnStyle.Render("2| ") + clean
	}
	return "1| " + clean
}

// execArgvLabel renders the command for a result line.
func execArgvLabel(argv []string) string { return cli.ExecArgvString(argv) }

// execResultLine renders how a command ended.
//
// A non-zero exit is a WARNING, not an error: the command ran and said what it
// meant, and `make test` returning 1 is a result the operator asked for. Only a
// command that never ran is red.
func execResultLine(taskID string, argv []string, res cli.ExecRunResult) string {
	head := fmt.Sprintf("exec %s: %s: ", pfShortID(taskID), execArgvLabel(argv))
	if res.Kind == protocol.ExecEventKind_Exited {
		if res.ExitCode == 0 {
			return head + OKStyle.Render("exit 0")
		}
		return head + WarnStyle.Render(fmt.Sprintf("exit %d", res.ExitCode))
	}
	tail := res.Kind.String()
	if res.Detail != "" {
		tail += ": " + res.Detail
	}
	return head + ErrorStyle.Render(tail)
}

// runExecRunAction dispatches one parsed `exec` command, resolving the task id
// prefix the way every other id-taking action here does.
func (a *App) runExecRunAction(v ExecRunAction) tea.Cmd {
	switch v.Sub {
	case "ls":
		filter := ""
		if v.TaskID != "" {
			full, errStr := a.resolveTaskIDPrefix(v.TaskID)
			if errStr != "" {
				a.cmdresult.Append(ErrorStyle.Render("exec ls: " + errStr))
				return nil
			}
			filter = full
		}
		return DoExecRunList(a.client, filter)
	case "kill":
		return DoExecRunKill(a.client, v.ExecID)
	default:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render("exec: " + errStr))
			return nil
		}
		a.cmdresult.Append(fmt.Sprintf("exec %s: %s …", pfShortID(full), execArgvLabel(v.Argv)))
		return DoExecRun(a.client, full, v.Argv, v.Shell, a.program)
	}
}

// DoExecRun runs one command in a task's worktree and streams its output into
// cmdresult. It uses the long-lived client (never dials), like every other Do*
// in this layer, and program MUST be App's *tea.Program.
func DoExecRun(c *cli.Client, taskID string, argv []string, shell bool, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return ExecRunDoneMsg{TaskID: taskID, Argv: argv, Err: fmt.Errorf("not connected to server")}
		}
		ch := make(chan ExecRunOutputMsg, 4096)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for {
				select {
				case <-ctx.Done():
					// Drain what is already queued so the tail of a command's
					// output is not lost to the shutdown itself.
					for {
						select {
						case m := <-ch:
							program.Send(m)
						default:
							return
						}
					}
				case m := <-ch:
					program.Send(m)
				}
			}
		}()

		var dropped atomic.Uint64
		out := &execLineWriter{ch: ch, dropped: &dropped}
		errw := &execLineWriter{ch: ch, dropped: &dropped, stderr: true}

		go func() {
			defer cancel()
			res, err := c.ExecRun(context.Background(), taskID, argv, cli.ExecRunOpts{
				ShellLine: shell,
				Stdout:    out,
				Stderr:    errw,
			})
			out.flush()
			errw.flush()
			program.Send(ExecRunDoneMsg{
				TaskID: taskID, Argv: argv, Result: res, Dropped: dropped.Load(), Err: err,
			})
		}()
		return nil
	}
}

// DoExecRunList fetches the running execs this operator may see.
func DoExecRunList(c *cli.Client, taskFilter string) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return ExecRunListMsg{Err: fmt.Errorf("not connected to server")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		execs, err := c.ExecRunListWith(ctx, taskFilter)
		return ExecRunListMsg{Execs: execs, Err: err}
	}
}

// DoExecRunKill stops one running exec by id. The registry is shared, so this
// reaches an exec another client started, exactly as `forward kill` does.
func DoExecRunKill(c *cli.Client, id uint64) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return ExecRunKillMsg{ExecID: id, Err: fmt.Errorf("not connected to server")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return ExecRunKillMsg{ExecID: id, Err: c.ExecRunKillWith(ctx, id)}
	}
}
