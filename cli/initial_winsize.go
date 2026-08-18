package cli

// windowSizeSetter is the one CommandExecutionStream method the initial-size
// send needs. Narrowed to an interface so the policy below is testable without
// a live stream and a running runner.
type windowSizeSetter interface {
	SetTerminalWindowSize(rows, columns, width, height uint16) error
}

// applyInitialWindowSize sends one TerminalWindowSize control frame for
// opts.InitialRows/InitialCols, or nothing when either is zero.
//
// Both-or-nothing rather than "fill the missing one with a default": a PTY
// sized 40x0 is not a smaller terminal, it is a broken one, and guessing the
// other half would hide a caller that only passed one flag.
//
// A send failure is returned, not swallowed. This is the first frame written to
// a stream that was just opened, so a failure here means the stream is dead and
// the session is unusable — reporting it as a warning would leave the caller
// holding something that cannot work.
func applyInitialWindowSize(s windowSizeSetter, opts SessionOpts) error {
	if opts.InitialRows == 0 || opts.InitialCols == 0 {
		return nil
	}
	return s.SetTerminalWindowSize(opts.InitialRows, opts.InitialCols, 0, 0)
}
