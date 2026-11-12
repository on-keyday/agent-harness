package server

import (
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runWatchTick is the run watcher's evaluation period, await-idle's.
const runWatchTick = idleWatchTick

// runWatchers holds one stop channel per background session being watched.
type runWatchers struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (h *TaskHandler) runWatchReg() *runWatchers {
	h.runWatchOnce.Do(func() { h.runWatchSet = &runWatchers{m: map[string]chan struct{}{}} })
	return h.runWatchSet
}

// armRunWatcher watches a background run on its mux (A-D13): every tick it
// gathers runInputs from the task and the mux and ends the run once
// evaluateRun says so. It leaves on a fire, when the mux stops, when the task
// is no longer an active background run, or on disarmRunWatcher. Re-arming
// replaces the previous watcher. Memory-only: a restart re-arms it at
// re-adoption.
func (h *TaskHandler) armRunWatcher(taskID string, mux *SessionMux) {
	reg := h.runWatchReg()
	stop := make(chan struct{})
	reg.mu.Lock()
	if old, ok := reg.m[taskID]; ok {
		close(old)
	}
	reg.m[taskID] = stop
	reg.mu.Unlock()
	release := func() {
		reg.mu.Lock()
		if reg.m[taskID] == stop {
			delete(reg.m, taskID)
		}
		reg.mu.Unlock()
	}

	go func() {
		defer release()
		t := time.NewTicker(runWatchTick)
		defer t.Stop()
		for {
			in, active := h.runInputsOf(taskID, mux)
			if !active {
				return
			}
			if reason, ok := evaluateRun(in); ok {
				select {
				case <-stop: // disarmed between the read and the fire
					return
				default:
				}
				h.endBackgroundRun(taskID, reason)
				return
			}
			select {
			case <-stop:
				return
			case <-mux.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// disarmRunWatcher stops the task's run watcher, if any (a switch to
// foreground, A-D19).
func (h *TaskHandler) disarmRunWatcher(taskID string) {
	reg := h.runWatchReg()
	reg.mu.Lock()
	if stop, ok := reg.m[taskID]; ok {
		close(stop)
		delete(reg.m, taskID)
	}
	reg.mu.Unlock()
}

// runInputsOf reads the task and its mux for evaluateRun; active is false once
// the task is no longer a background run in progress.
func (h *TaskHandler) runInputsOf(taskID string, mux *SessionMux) (runInputs, bool) {
	t, ok := h.Tasks.Get(taskID)
	if !ok || t.RunMode != protocol.RunMode_Background {
		return runInputs{}, false
	}
	switch t.Status {
	case protocol.TaskStatus_Running, protocol.TaskStatus_Detached, protocol.TaskStatus_Held:
	default:
		return runInputs{}, false
	}
	var runStart int64
	if t.StartedAt != nil {
		runStart = t.StartedAt.UnixNano()
	}
	pst, _, pat, pwat := mux.programSnapshot()
	lo := mux.LastOutputUnixNano()
	return runInputs{
		Now:              time.Now().UnixNano(),
		RunStart:         runStart,
		SwitchedAt:       t.ModeSwitchedAt,
		MuxStart:         mux.MuxStartUnixNano(),
		LastInput:        mux.LastInputUnixNano(),
		LastOutput:       lo,
		Kind:             t.Kind,
		Cond:             t.Cond,
		Life:             LifecycleReading{State: t.Lifecycle, At: t.LifecycleAt, WorkingAt: t.LifecycleWorkingAt},
		Prog:             pst,
		ProgAt:           pat,
		ProgWorkingAt:    max(pwat, mux.carriedProgramWorkingAt.Load()),
		SawOutputThisRun: mux.sawOutputSince(runStart),
	}, true
}
