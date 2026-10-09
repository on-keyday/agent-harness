package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// TapLinesMsg carries already-rendered lines from a pump into the view.
// Rendering happens in cli (RenderTapRecord / RenderExecTapRecord), so this
// surface cannot drift from what harness-cli prints.
type TapLinesMsg struct {
	Subject tapSubject
	Lines   []string
}

// TapEndedMsg reports that a tap stopped, and why. Err is nil for a clean end
// (the subject ended, or the operator closed it).
type TapEndedMsg struct {
	Subject tapSubject
	Err     error
}

// openTap opens the view on subject and runs stream in the background until it
// returns. One tap at a time: the pane it is opened from selects one row. It
// runs on a.client — the long-lived connection — never a fresh dial.
func (a *App) openTap(subject tapSubject, stream func(ctx context.Context, onLines func([]string)) error) tea.Cmd {
	a.stopTap()
	a.tap = NewTapView(subject)
	a.tap.SetSize(a.width, a.height)
	a.tap.Open()
	ctx, cancel := context.WithCancel(context.Background())
	a.tapStop = cancel
	program := a.program
	return func() tea.Msg {
		err := stream(ctx, func(lines []string) {
			if program != nil {
				program.Send(TapLinesMsg{Subject: subject, Lines: lines})
			}
		})
		if ctx.Err() != nil {
			return nil // the operator closed it
		}
		return TapEndedMsg{Subject: subject, Err: err}
	}
}

func (a *App) startForwardTap(v verb.ForwardTapAction) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(ErrorStyle.Render("forward tap: not connected to server"))
		return nil
	}
	filter, err := cli.ParseTapFilter(v.Dir)
	if err != nil {
		a.cmdresult.Append(ErrorStyle.Render(err.Error()))
		return nil
	}
	client := a.client
	opts := cli.ForwardTapOpts{Filter: filter, MaxRecordBytes: v.MaxRecordBytes, Mode: cli.TapHex}
	return a.openTap(tapSubject{"forward", v.ForwardID}, func(ctx context.Context, onLines func([]string)) error {
		return cli.StreamForwardTap(ctx, client, v.ForwardID, opts, onLines)
	})
}

func (a *App) startExecTap(v verb.ExecTapAction) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(ErrorStyle.Render("exec tap: not connected to server"))
		return nil
	}
	filter, err := cli.ParseExecTapFilter(v.Chan)
	if err != nil {
		a.cmdresult.Append(ErrorStyle.Render(err.Error()))
		return nil
	}
	client := a.client
	// The view is lines, so the declaration keeps --raw off this surface;
	// hex, text and json render here as they print on the CLI.
	opts := cli.ExecTapOpts{Filter: filter, MaxRecordBytes: v.MaxRecordBytes, Mode: cli.TapModeByName(v.Mode)}
	return a.openTap(tapSubject{"exec", v.ExecID}, func(ctx context.Context, onLines func([]string)) error {
		return cli.StreamExecTap(ctx, client, v.ExecID, opts, onLines)
	})
}

// stopTap closes the view and ends the pump. Idempotent.
func (a *App) stopTap() {
	if a.tapStop != nil {
		a.tapStop()
		a.tapStop = nil
	}
	a.tap.Close()
}
