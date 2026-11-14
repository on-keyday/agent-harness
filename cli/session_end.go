package cli

import (
	"fmt"
	"io"
	"time"
)

// AttachedStream is an attached session's stream as the end-of-attach report
// reads it (objtrsf exec.CommandExecutionStream).
type AttachedStream interface {
	DetachedByKey() bool
	EndReason() (reason string, exitCode int32, ok bool)
	Stdout() io.Reader
}

// attachEnd reads why the far side ended an attached session, from the End
// frame it sends ahead of the close. A detach is not an end. When the attach
// failed — a runner refusing the open can close the stream before the client
// has sent its size, so a send fails on a session already over — the stream is
// read to its close first, since the End is still on the read side ahead of
// it; the bound only guards a read side that never ends.
func attachEnd(s AttachedStream, shellErr error) (reason string, exitCode int32, ok bool) {
	if s.DetachedByKey() {
		return "", 0, false
	}
	if shellErr != nil {
		drained := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, s.Stdout())
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
		}
	}
	return s.EndReason()
}

// AttachEndReason is attachEnd's reason for a front end that reports it
// itself (the TUI).
func AttachEndReason(s AttachedStream, shellErr error) (string, bool) {
	reason, _, ok := attachEnd(s, shellErr)
	return reason, ok
}

// finishAttach is an attach's result once its session returned. An End with
// exit -1 means nothing ran — the runner refused the open — so the open
// failed with that reason, however the attach itself ended. An End from a
// session that ran is printed. Without one the shell's own result stands.
func finishAttach(s AttachedStream, shellErr error, w io.Writer) error {
	reason, exitCode, ok := attachEnd(s, shellErr)
	if !ok {
		return shellErr
	}
	if exitCode == -1 {
		return fmt.Errorf("interactive session_ended: %s", reason)
	}
	fmt.Fprintln(w, "harness-cli: session ended: "+reason)
	return nil
}
