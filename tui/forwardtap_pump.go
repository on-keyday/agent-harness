package tui

import (
	"context"
	"github.com/on-keyday/agent-harness/cli/verb"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// ForwardTapLinesMsg carries already-rendered lines from the pump into the
// view. Rendering happens in cli.RenderTapRecord, off the shared renderer, so
// this surface cannot drift from what `harness-cli forward tap` prints.
type ForwardTapLinesMsg struct {
	ForwardID uint64
	Lines     []string
}

// ForwardTapEndedMsg reports that the tap stopped, and why. err is nil for a
// clean end (the forward closed, or the operator did).
type ForwardTapEndedMsg struct {
	ForwardID uint64
	Err       error
}

// startForwardTap opens the view and starts the pump. It runs on a.client —
// the long-lived connection this TUI already holds — rather than dialling: a
// fresh dial here would throw away a handshake, which is the pattern every
// other Do* in this package follows.
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
	// One tap at a time: a second would need a second view, and the pane it is
	// opened from selects exactly one row.
	a.stopForwardTap()

	a.forwardTap = NewForwardTapView(v.ForwardID)
	a.forwardTap.SetSize(a.width, a.height)
	a.forwardTap.Open()

	ctx, cancel := context.WithCancel(context.Background())
	a.forwardTapStop = cancel

	client := a.client
	forwardID := v.ForwardID
	opts := cli.ForwardTapOpts{Filter: filter, MaxRecordBytes: v.MaxRecordBytes, Mode: cli.TapHex}
	program := a.program

	return func() tea.Msg {
		err := cli.StreamForwardTap(ctx, client, forwardID, opts, func(lines []string) {
			if program != nil {
				program.Send(ForwardTapLinesMsg{ForwardID: forwardID, Lines: lines})
			}
		})
		if ctx.Err() != nil {
			// The operator closed it. Not a failure, and the view is already
			// gone by the time this lands.
			return nil
		}
		return ForwardTapEndedMsg{ForwardID: forwardID, Err: err}
	}
}

// stopForwardTap closes the view and ends the pump. Idempotent: Esc on a tap
// whose forward already ended must not panic on a nil cancel.
func (a *App) stopForwardTap() {
	if a.forwardTapStop != nil {
		a.forwardTapStop()
		a.forwardTapStop = nil
	}
	a.forwardTap.Close()
}
