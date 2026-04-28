//go:build !js

package exec

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	pty "github.com/aymanbagabas/go-pty"
	"github.com/on-keyday/agent-harness/exec/frame"
	"github.com/on-keyday/agent-harness/trsf"

	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

type outStreamWrapper struct {
	frameType frame.FrameType
	s         trsf.BidirectionalStream
}

func (o *outStreamWrapper) Write(p []byte) (n int, err error) {
	originLen := len(p)
	for len(p) > 0 {
		chunkSize := len(p)
		chunck := min(chunkSize, math.MaxUint32)
		dataToSend := p[:chunck]
		p = p[chunck:]
		// wrapping with frame
		hdr := frame.FrameHeader{
			Type: o.frameType,
			Len:  uint32(len(dataToSend)),
		}
		var dataCopy []byte // because p will be changed in next loop
		if len(dataToSend) > 0 {
			dataCopy = make([]byte, len(dataToSend))
			copy(dataCopy, dataToSend)
		}
		err = o.s.AppendData(false, hdr.MustAppend(nil), dataCopy)
		if err != nil {
			return 0, err
		}
	}
	return originLen, nil
}

func (c *outStreamWrapper) Close() error {
	hdr := frame.FrameHeader{
		Type: c.frameType,
		Len:  0,
	}
	return c.s.AppendData(true, hdr.MustAppend(nil))
}

// resizePty applies a window-size update to the given Pty. On Unix it uses
// the UnixPty extension to also propagate pixel dimensions (Xpixel/Ypixel),
// which some TUIs use for inline image / sixel sizing. On Windows ConPTY
// has no pixel concept and we fall back to the cell-only Resize.
func resizePty(p pty.Pty, rows, cols, width, height uint16) error {
	if up, ok := p.(pty.UnixPty); ok {
		return up.SetWinsize(&pty.Winsize{
			Row:    rows,
			Col:    cols,
			Xpixel: width,
			Ypixel: height,
		})
	}
	return p.Resize(int(cols), int(rows))
}

func ExecuteCommand(ctx context.Context, stream trsf.BidirectionalStream, logger *slog.Logger, command string, args []string, cwd string, ptyEnabled bool) error {
	defer stream.CloseBoth()
	logger.Info("Executing command", "command", command, "args", args, "cwd", cwd, "pty", ptyEnabled)
	gr, grCtx := errgroup.WithContext(ctx)
	gr.SetLimit(-1)
	stdout := &outStreamWrapper{
		frameType: frame.FrameType_Stdout,
		s:         stream,
	}
	stderr := &outStreamWrapper{
		frameType: frame.FrameType_Stderr,
		s:         stream,
	}
	pipeOut, pipeIn := io.Pipe()
	var ptyHandle pty.Pty
	var process *os.Process
	var waitFn func() error
	handleInput := func() error {
		defer pipeIn.Close()
		for {
			hdr := &frame.Frame{}
			err := hdr.Read(stream)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if hdr.Header.Type == frame.FrameType_Stdin {
				if hdr.Header.Len == 0 { // close stdin
					pipeIn.Close()
					continue
				}
				data := *hdr.Data()
				_, err = pipeIn.Write(data)
				if err != nil {
					return err
				}
			} else if ctrl := hdr.Control(); ctrl != nil {
				switch ctrl.Type {
				case frame.ControlType_TerminalWindowSize:
					if ptyHandle == nil {
						logger.Warn("received terminal window size control frame, but pty is not enabled")
						continue
					}
					ws := ctrl.TerminalWindowSize()
					if err := resizePty(ptyHandle, ws.Rows, ws.Columns, ws.Width, ws.Height); err != nil {
						logger.Error("failed to set pty window size", "error", err)
					}
				case frame.ControlType_Signal:
					sig := ctrl.Signal()
					if process == nil {
						logger.Warn("received signal control frame before process start", "signal", sig.Signal)
						continue
					}
					if err := process.Signal(syscall.Signal(sig.Signal)); err != nil {
						logger.Error("failed to send signal to process", "error", err)
					}
				default:
					logger.Warn("unknown control frame received", "type", ctrl.Type)
				}
			} else {
				logger.Warn("unknown frame type received", "type", hdr.Header.Type)
			}
		}
	}
	var procExited atomic.Bool
	if ptyEnabled {
		p, err := pty.New()
		if err != nil {
			return err
		}
		defer p.Close()
		ptyCmd := p.CommandContext(grCtx, command, args...)
		if cwd != "" {
			ptyCmd.Dir = cwd
		}
		if err := ptyCmd.Start(); err != nil {
			return err
		}
		ptyHandle = p
		process = ptyCmd.Process
		waitFn = ptyCmd.Wait
		gr.Go(func() error {
			defer p.Close()
			_, err := io.Copy(p, pipeOut)
			// try SIGHUP to notify EOF
			if process != nil {
				process.Signal(syscall.SIGHUP)
				// try SIGTERM after 1 second if not exited
				time.AfterFunc(1*time.Second, func() {
					if !procExited.Load() && process != nil {
						process.Signal(syscall.SIGTERM)
						// finally try SIGKILL after another 1 second
						time.AfterFunc(1*time.Second, func() {
							if !procExited.Load() && process != nil {
								process.Kill()
							}
						})
					}
				})
			}
			return err
		})
		gr.Go(func() error {
			defer stdout.Close()
			_, err := io.Copy(stdout, p)
			return err
		})
	} else {
		cmd := exec.CommandContext(grCtx, command, args...)
		if cwd != "" {
			cmd.Dir = cwd
		}
		cmd.Stdin = pipeOut
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		process = cmd.Process
		waitFn = cmd.Wait
	}
	gr.Go(handleInput)
	gr.Go(func() error {
		defer stream.Cancel() // terminate the input handler
		err := waitFn()
		procExited.Store(true)
		return err
	})
	err := gr.Wait()
	if err != nil {
		logger.Error("command execution stream ended with error", "error", err)
	} else {
		logger.Info("command execution stream ended")
	}
	return nil
}

type CommandExecutionStream struct {
	trsf.BidirectionalStream
	stdoutPipe *io.PipeReader
	stderrPipe *io.PipeReader
}

func NewCommandExecutionStream(stream trsf.BidirectionalStream) *CommandExecutionStream {
	stdoutPipeR, stdoutPipeW := io.Pipe()
	stderrPipeR, stderrPipeW := io.Pipe()
	go func() {
		defer stdoutPipeW.Close()
		defer stderrPipeW.Close()
		for {
			hdr := &frame.Frame{}
			err := hdr.Read(stream)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				stdoutPipeW.CloseWithError(err)
				stderrPipeW.CloseWithError(err)
				return
			}
			switch hdr.Header.Type {
			case frame.FrameType_Stdout:
				if hdr.Header.Len == 0 {
					stdoutPipeW.Close()
					continue
				}
				data := *hdr.Data()
				_, err = stdoutPipeW.Write(data)
				if err != nil {
					stdoutPipeW.CloseWithError(err)
					return
				}
			case frame.FrameType_Stderr:
				if hdr.Header.Len == 0 {
					stderrPipeW.Close()
					continue
				}
				data := *hdr.Data()
				_, err = stderrPipeW.Write(data)
				if err != nil {
					stderrPipeW.CloseWithError(err)
					return
				}
			default:
				// ignore unknown frame types
			}
		}
	}()
	return &CommandExecutionStream{
		BidirectionalStream: stream,
		stdoutPipe:          stdoutPipeR,
		stderrPipe:          stderrPipeR,
	}
}

func (w *CommandExecutionStream) Stdout() io.Reader {
	return w.stdoutPipe
}

func (w *CommandExecutionStream) Stderr() io.Reader {
	return w.stderrPipe
}

func (w *CommandExecutionStream) Stdin() io.Writer {
	return &stdinWrapper{
		s: w.BidirectionalStream,
	}
}

type stdinWrapper struct {
	s trsf.BidirectionalStream
}

func (w *stdinWrapper) Close() error {
	hdr := frame.FrameHeader{
		Type: frame.FrameType_Stdin,
		Len:  0,
	}
	return w.s.AppendData(false, hdr.MustAppend(nil))
}

func (w *stdinWrapper) Write(data []byte) (n int, err error) {
	hdr := frame.FrameHeader{
		Type: frame.FrameType_Stdin,
		Len:  uint32(len(data)),
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	err = w.s.AppendData(false, hdr.MustAppend(nil), copied)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *CommandExecutionStream) SendSignal(sig syscall.Signal) error {
	ctrl := frame.Control{
		Type: frame.ControlType_Signal,
	}
	ctrl.SetSignal(frame.Signal{
		Signal: int32(sig),
	})
	enc := ctrl.MustAppend(nil)
	fullCtrl := frame.FrameHeader{
		Type: frame.FrameType_Control,
		Len:  uint32(len(enc)),
	}
	return w.AppendData(false, fullCtrl.MustAppend(nil), enc)
}

func (w *CommandExecutionStream) SetTerminalWindowSize(rows, columns, width, height uint16) error {
	ctrl := frame.Control{
		Type: frame.ControlType_TerminalWindowSize,
	}
	ctrl.SetTerminalWindowSize(frame.TerminalWindowSize{
		Rows:    rows,
		Columns: columns,
		Width:   width,
		Height:  height,
	})
	enc := ctrl.MustAppend(nil)
	fullCtrl := frame.FrameHeader{
		Type: frame.FrameType_Control,
		Len:  uint32(len(enc)),
	}
	return w.AppendData(false, fullCtrl.MustAppend(nil), enc)
}

func (w *CommandExecutionStream) Close() error {
	w.stdoutPipe.Close()
	w.stderrPipe.Close()
	w.BidirectionalStream.Cancel()
	return w.BidirectionalStream.Close()
}

func (w *CommandExecutionStream) RemoteShell() error {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), old)

	// sendSize re-queries the local terminal dimensions and forwards them
	// over the control frame channel. Used both for the initial size and
	// for every SIGWINCH thereafter.

	if err := w.sendWindowSize(); err != nil {
		return err
	}

	// Window-size forwarding: when the local terminal resizes, push a
	// fresh TerminalWindowSize control frame so the runner-side PTY (and
	// claude inside it) sees the new dimensions and re-flows. Without
	// this, claude renders at the dimensions captured at attach time and
	// stays frozen for the rest of the session even if the user resizes
	// their terminal. Detection is platform-specific: SIGWINCH on Unix,
	// polling on Windows — see winsize_{unix,windows}.go.
	stopWinSize := startWindowSizeForwarder(w.sendWindowSize)
	defer stopWinSize()

	stdin := w.Stdin()
	stdout := w.Stdout()
	go io.Copy(stdin, os.Stdin)
	_, err = io.Copy(os.Stdout, stdout)
	return err
}
