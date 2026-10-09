package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/trsf"
)

// spliceExecCounted pumps an exec's frame stream between the client and the
// runner, counting payload per channel and offering it to the exec's taps on
// the way past.
//
// Teardown stays half-close, for the reason handleOpenExecRun gives: a command
// that finishes in milliseconds must not have the client's data stream torn
// down before the client has resolved it by id.
//
// The caller calls e.beginOutput() BEFORE starting it: an exec that ends before
// this goroutine is scheduled must still find its output marked pending.
func spliceExecCounted(client, runner trsf.BidirectionalStream, e *execRun) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); relayExecFrames(client, runner, e) }()
	// The runner→client relay carries the exec's last output, which can arrive
	// after the runner reported the end; the taps' exec_ended waits for it.
	// beginOutput was called by the caller, before this goroutine existed.
	go func() { defer wg.Done(); defer e.outputDone(); relayExecFrames(runner, client, e) }()
	wg.Wait()
	_ = client.CloseBoth()
	_ = runner.CloseBoth()
	slog.Info("exec_run: splice ended", "exec_id", e.execID, "task_id", e.taskIDHex)
}

// relayExecFrames is relayBytes with a frame scanner attached. The scanner
// reads only headers and lengths, so it stays as cheap as the loop it rides on;
// the bytes are forwarded unchanged whatever it concluded.
func relayExecFrames(src, dst trsf.BidirectionalStream, e *execRun) {
	var sc frameScanner
	for {
		data, eof, err := src.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			sc.scan(data, e.observeFrame, e.observeEmptyFrame)
			if werr := dst.AppendData(eof, data); werr != nil {
				return
			}
		} else if eof {
			_ = dst.AppendData(true)
		}
		if eof {
			return
		}
	}
}

func (e *execRun) observeFrame(t frame.FrameType, data []byte) {
	if ch, ok := execTapChannelOf(t); ok {
		e.notePayload(ch, data)
	}
}

func (e *execRun) observeEmptyFrame(t frame.FrameType) {
	if ch, ok := execTapChannelOf(t); ok {
		e.noteEOF(ch)
	}
}

func (e *execRun) counter(ch protocol.ExecTapChannel) *atomic.Uint64 {
	switch ch {
	case protocol.ExecTapChannel_Stdout:
		return &e.stdoutBytes
	case protocol.ExecTapChannel_Stderr:
		return &e.stderrBytes
	}
	return &e.stdinBytes
}

// notePayload counts data on ch and offers it to every tap at the channel's
// offset BEFORE these bytes. Each channel is written by one relay goroutine
// (stdin by client→runner, stdout and stderr by runner→client), so the offset
// read here is the one these bytes start at.
func (e *execRun) notePayload(ch protocol.ExecTapChannel, data []byte) {
	n := uint64(len(data))
	offset := e.counter(ch).Add(n) - n
	e.lastActivityMs.Store(time.Now().UnixMilli())
	e.eachTap(func(t *execTap) { t.offer(ch, offset, data) })
}

func (e *execRun) noteEOF(ch protocol.ExecTapChannel) {
	e.eachTap(func(t *execTap) { t.emitEOF(ch) })
}

// counters reads the set the listing renders.
func (e *execRun) counters() (stdin, stdout, stderr, lastMs uint64) {
	last := e.lastActivityMs.Load()
	if last < 0 {
		last = 0
	}
	return e.stdinBytes.Load(), e.stdoutBytes.Load(), e.stderrBytes.Load(), uint64(last)
}
