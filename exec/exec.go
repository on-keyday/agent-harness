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

	"github.com/creack/pty"
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

func ExecuteCommand(ctx context.Context, stream trsf.BidirectionalStream, logger *slog.Logger, command string, args []string, cwd string, ptyEnabled bool) error {
	defer stream.CloseBoth()
	logger.Info("Executing command", "command", command, "args", args, "cwd", cwd, "pty", ptyEnabled)
	gr, grCtx := errgroup.WithContext(ctx)
	gr.SetLimit(-1)
	cmd := exec.CommandContext(grCtx, command, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdout := &outStreamWrapper{
		frameType: frame.FrameType_Stdout,
		s:         stream,
	}
	stderr := &outStreamWrapper{
		frameType: frame.FrameType_Stderr,
		s:         stream,
	}
	pipeOut, pipeIn := io.Pipe()
	var ptyFile *os.File
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
					if ptyFile == nil {
						logger.Warn("received terminal window size control frame, but pty is not enabled")
						continue
					}
					ws := ctrl.TerminalWindowSize()
					err := pty.Setsize(ptyFile, &pty.Winsize{
						Cols: ws.Columns,
						Rows: ws.Rows,
						X:    ws.Width,
						Y:    ws.Height,
					})
					if err != nil {
						logger.Error("failed to set pty window size", "error", err)
					}
				case frame.ControlType_Signal:
					sig := ctrl.Signal()
					err := cmd.Process.Signal(syscall.Signal(sig.Signal))
					if err != nil {
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
		tty, err := pty.Start(cmd)
		if err != nil {
			return err
		}
		defer tty.Close()
		ptyFile = tty
		gr.Go(func() error {
			defer tty.Close()
			_, err := io.Copy(tty, pipeOut)
			// try SIGHUP to notify EOF
			if cmd.Process != nil {
				cmd.Process.Signal(syscall.SIGHUP)
				// try SIGTERM after 1 second if not exited
				time.AfterFunc(1*time.Second, func() {
					if !procExited.Load() && cmd.Process != nil {
						cmd.Process.Signal(syscall.SIGTERM)
						// finally try SIGKILL after another 1 second
						time.AfterFunc(1*time.Second, func() {
							if !procExited.Load() && cmd.Process != nil {
								cmd.Process.Kill()
							}
						})
					}
				})
			}
			return err
		})
		gr.Go(func() error {
			defer stdout.Close()
			_, err := io.Copy(stdout, tty)
			return err
		})
	} else {
		cmd.Stdin = pipeOut
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err := cmd.Start()
		if err != nil {
			return err
		}
	}
	gr.Go(handleInput)
	gr.Go(func() error {
		defer stream.Cancel() // terminate the input handler
		err := cmd.Wait()
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

	fullSize, err := pty.GetsizeFull(os.Stdin)

	err = w.SetTerminalWindowSize(fullSize.Rows, fullSize.Cols, fullSize.X, fullSize.Y)
	if err != nil {
		return err
	}
	stdin := w.Stdin()
	stdout := w.Stdout()
	go io.Copy(stdin, os.Stdin)
	_, err = io.Copy(os.Stdout, stdout)
	return err
}
