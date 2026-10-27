package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerKeyResultMsg carries the result of runner enroll / keys / unenroll,
// already rendered by the cli.FormatRunner* function the CLI prints with.
type RunnerKeyResultMsg struct {
	Verb    string
	Text    string
	OK      bool
	Err     error
	Refresh bool // the registry may have changed: re-read the snapshot
}

func runnerKeyCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func DoRunnerEnroll(c *cli.Client, pubkey, label string) tea.Cmd {
	return func() tea.Msg {
		key, err := hex.DecodeString(pubkey)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner enroll", Err: err}
		}
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		st, err := cli.RunnerEnrollWith(ctx, c, key, label)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner enroll", Err: err}
		}
		ok := st == protocol.RunnerKeyStatus_Ok || st == protocol.RunnerKeyStatus_AlreadyEnrolled
		return RunnerKeyResultMsg{Verb: "runner enroll", Text: cli.FormatRunnerEnroll(st, key, label), OK: ok, Refresh: ok}
	}
}

func DoRunnerKeys(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		rows, err := cli.RunnerKeysWith(ctx, c)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner keys", Err: err}
		}
		return RunnerKeyResultMsg{Verb: "runner keys", Text: cli.FormatRunnerKeys(rows), OK: true}
	}
}

func DoRunnerUnenroll(c *cli.Client, prefix string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		st, res, err := cli.RunnerUnenrollWith(ctx, c, prefix, force)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner unenroll", Err: err}
		}
		text, ok := cli.FormatRunnerUnenroll(st, res)
		return RunnerKeyResultMsg{Verb: "runner unenroll", Text: text, OK: ok, Refresh: ok}
	}
}

// appendRunnerKeyResult writes a RunnerKeyResultMsg to the command result
// pane, one line per line of the rendered text.
func (a *App) appendRunnerKeyResult(msg RunnerKeyResultMsg) {
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("%s: %v", msg.Verb, msg.Err)))
		return
	}
	style := OKStyle
	if !msg.OK {
		style = ErrorStyle
	}
	for _, line := range strings.Split(strings.TrimRight(msg.Text, "\n"), "\n") {
		a.cmdresult.Append(style.Render(line))
	}
}
