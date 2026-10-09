package server

import (
	"context"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// statsSweepInterval is how often the sweep looks for counters that moved, on
// forwards and execs.
//
// The quantity underneath moves per BYTE, so it cannot be published per change
// the way a conn's open/close can. One sweep coalesces: a forward carrying a
// file transfer produces one event per interval, not one per chunk, and a
// forward carrying nothing produces none at all — which is the property that
// makes this cheaper than the clients polling, not just tidier.
const statsSweepInterval = time.Second

// publishedCounters is the last set published for a forward. The sweep compares
// against it so an idle forward costs one comparison and no traffic.
type publishedCounters struct {
	toTarget   uint64
	fromTarget uint64
	connsTotal uint64
	connsOpen  uint32
	taps       uint16
	valid      bool
}

func (pc publishedCounters) sameAs(to, from, total uint64, open uint32, taps uint16) bool {
	return pc.valid && pc.toTarget == to && pc.fromTarget == from &&
		pc.connsTotal == total && pc.connsOpen == open && pc.taps == taps
}

// emitForwardEvent raises the hook the server wires to forwards.status. nil
// hook (tests, and any wiring without a pubsub) is a no-op.
func (h *TaskHandler) emitForwardEvent(kind protocol.StatusEventKind, pf *portForward) {
	if h == nil || h.OnForwardEvent == nil || pf == nil {
		return
	}
	h.OnForwardEvent(kind, pf)
}

// emitExecEvent is its sibling for execs.status.
func (h *TaskHandler) emitExecEvent(kind protocol.StatusEventKind, e *execRun) {
	if h == nil || h.OnExecEvent == nil || e == nil {
		return
	}
	h.OnExecEvent(kind, e)
}

// removeExec is the single funnel for dropping an exec registration, so
// exec_ended fires — on execs.status and to every tap — from every path that
// ends one: the send-failure rollback, the child finishing, an explicit kill,
// and the client's connection going away. The outcome is a parameter rather
// than a call each site must remember: a site that omits it does not compile.
func (h *TaskHandler) removeExec(execID uint64, kind protocol.ExecEventKind, exitCode int32) (*execRun, bool) {
	e, ok := h.execs().remove(execID)
	if ok {
		h.emitExecEvent(protocol.StatusEventKind_ExecEnded, e)
		e.endTaps(kind, exitCode)
	}
	return e, ok
}

// runStatsSweeper publishes forward_stats and exec_stats for the forwards and
// execs whose counters moved since the last sweep.
//
// One goroutine for the whole server rather than one per forward: coalescing is
// then structural instead of something each publisher has to remember, and the
// cost of an idle server is one walk of a short list per interval.
func (h *TaskHandler) runStatsSweeper(ctx context.Context) {
	t := time.NewTicker(statsSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweepForwardStats()
			h.sweepExecStats()
		}
	}
}

func (h *TaskHandler) sweepForwardStats() {
	if h.OnForwardEvent == nil {
		return
	}
	for _, pf := range h.pforwards().list() {
		to, from, total, open, _ := pf.counters()
		taps := pf.tapCount()
		pf.statsMu.Lock()
		unchanged := pf.lastPublished.sameAs(to, from, total, open, taps)
		if !unchanged {
			pf.lastPublished = publishedCounters{
				toTarget: to, fromTarget: from, connsTotal: total,
				connsOpen: open, taps: taps, valid: true,
			}
		}
		pf.statsMu.Unlock()
		if unchanged {
			continue
		}
		h.emitForwardEvent(protocol.StatusEventKind_ForwardStats, pf)
	}
}

// forwardStatusPayload / execStatusPayload encode one event. Split out so the
// server's publisher stays a wiring function and the shape lives beside the
// rest of the event code.
func forwardStatusPayload(kind protocol.StatusEventKind, pf *portForward) []byte {
	ev := protocol.ForwardStatusEvent{
		Kind: kind,
		Ts:   uint64(time.Now().UnixNano()),
		Info: portForwardInfo(pf),
	}
	return ev.MustAppend(nil)
}

func execStatusPayload(kind protocol.StatusEventKind, e *execRun) []byte {
	ev := protocol.ExecStatusEvent{
		Kind: kind,
		Ts:   uint64(time.Now().UnixNano()),
		Info: execRunInfo(e),
	}
	return ev.MustAppend(nil)
}

func (pc publishedExecCounters) sameAs(stdin, stdout, stderr uint64, taps uint16) bool {
	return pc.valid && pc.stdin == stdin && pc.stdout == stdout && pc.stderr == stderr && pc.taps == taps
}

// sweepExecStats is sweepForwardStats for execs: publish exec_stats for each
// exec whose counters or tap count changed since its last publish.
func (h *TaskHandler) sweepExecStats() {
	if h.OnExecEvent == nil {
		return
	}
	for _, e := range h.execs().list("") {
		in, out, errb, _ := e.counters()
		taps := e.tapCount()
		e.statsMu.Lock()
		unchanged := e.lastPublished.sameAs(in, out, errb, taps)
		if !unchanged {
			e.lastPublished = publishedExecCounters{stdin: in, stdout: out, stderr: errb, taps: taps, valid: true}
		}
		e.statsMu.Unlock()
		if unchanged {
			continue
		}
		h.emitExecEvent(protocol.StatusEventKind_ExecStats, e)
	}
}
