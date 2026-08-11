package tui

import (
	"errors"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// errNoProgram is returned instead of touching the terminal when no
// tea.Program is bound (App constructed without BindProgram — tests).
var errNoProgram = errors.New("no bound tea.Program: cannot release the terminal")

// runReleasingTerminal hands the terminal to c and runs it WITHOUT suspending
// the bubbletea event loop.
//
// tea.Exec performs the same three steps — ReleaseTerminal, Run,
// RestoreTerminal (bubbletea/exec.go:102-131) — but runs them inline on the
// event-loop goroutine, the same goroutine that reads p.msgs and calls Update
// ("NB: this blocks", bubbletea/tea.go:470). For a suspension measured in
// hours (an attached session) that costs two things: the model stops
// advancing, so the TUI returns with an hours-stale snapshot; and every
// program.Send in the process parks, because msgs is unbuffered. Parked
// senders are not merely cosmetic — the trsf run loop logs through
// SlogTailHandler, so a single Error-level record froze the whole stream
// plane until the user detached.
//
// Driven from a tea.Cmd goroutine instead, the event loop never leaves its
// select: Update keeps running, Send never blocks, and only rendering stops.
// ReleaseTerminal stops the renderer, after which the event loop's
// p.renderer.write(model.View()) fills a buffer that write() resets on every
// call (standard_renderer.go:303-306) — nothing accumulates, nothing paints
// over the child.
//
// Input belongs to the child for the duration: ReleaseTerminal cancels
// bubbletea's input reader, so no KeyMsg reaches Update until RestoreTerminal.
//
// p.exec additionally calls the unexported renderer.resetLinesRendered, which
// we cannot. It is irrelevant here: that counter only drives cursor movement
// OUTSIDE the alt screen (standard_renderer.go:174-177), and RestoreTerminal's
// enterAltScreen already zeroes the alt-screen counter and forces a full
// repaint (standard_renderer.go:370-373). harness-tui always runs under
// tea.WithAltScreen.
//
// os.Stdin/Stdout/Stderr are exactly what tea.Exec would pass (Program.input /
// Program.output), because cmd/harness-tui builds the Program without
// WithInput/WithOutput. A future option there must be threaded through here.
//
// NOT re-entrant: ReleaseTerminal/RestoreTerminal do not nest. Every caller
// must gate on App.termReleased — set it on the Update goroutine before
// returning the Cmd, clear it when the done message lands.
func runReleasingTerminal(p *tea.Program, c tea.ExecCommand) error {
	if err := p.ReleaseTerminal(); err != nil {
		return err
	}
	c.SetStdin(os.Stdin)
	c.SetStdout(os.Stdout)
	c.SetStderr(os.Stderr)
	runErr := c.Run()
	if rerr := p.RestoreTerminal(); rerr != nil && runErr == nil {
		runErr = rerr
	}
	return runErr
}

// execWithoutSuspend is tea.Exec's shape backed by runReleasingTerminal, so
// call sites read the same as the tea.Exec they replaced.
func execWithoutSuspend(p *tea.Program, c tea.ExecCommand, fn tea.ExecCallback) tea.Cmd {
	return func() tea.Msg {
		if p == nil {
			return fn(errNoProgram)
		}
		return fn(runReleasingTerminal(p, c))
	}
}
