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

// The dial-record and kick verbs answer through the same message: the text is
// cli.FormatRunner*'s, as the CLI prints it.

func DoRunnerDialAdd(c *cli.Client, candidates, keyPrefix, label string) tea.Cmd {
	return func() tea.Msg {
		cands, err := cli.SplitCandidates(candidates)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner dial add", Err: err}
		}
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		st, id, err := cli.RunnerDialAddWith(ctx, c, cands, keyPrefix, label)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner dial add", Err: err}
		}
		text, ok := cli.FormatRunnerDialAdd(st, id, cands)
		return RunnerKeyResultMsg{Verb: "runner dial add", Text: text, OK: ok}
	}
}

func DoRunnerDialList(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		rows, err := cli.RunnerDialListWith(ctx, c)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner dial ls", Err: err}
		}
		return RunnerKeyResultMsg{Verb: "runner dial ls", Text: cli.FormatRunnerDialList(rows), OK: true}
	}
}

func DoRunnerDialRemove(c *cli.Client, idText string) tea.Cmd {
	return func() tea.Msg {
		id, err := cli.ParseDialID(idText)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner dial rm", Err: err}
		}
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		st, err := cli.RunnerDialRemoveWith(ctx, c, id)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner dial rm", Err: err}
		}
		text, ok := cli.FormatRunnerDialRemove(st, id)
		return RunnerKeyResultMsg{Verb: "runner dial rm", Text: text, OK: ok}
	}
}

func DoRunnerKick(c *cli.Client, prefix string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := runnerKeyCtx()
		defer cancel()
		st, res, err := cli.RunnerKickWith(ctx, c, prefix, force)
		if err != nil {
			return RunnerKeyResultMsg{Verb: "runner kick", Err: err}
		}
		text, ok := cli.FormatRunnerKick(st, res)
		return RunnerKeyResultMsg{Verb: "runner kick", Text: text, OK: ok, Refresh: ok}
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
