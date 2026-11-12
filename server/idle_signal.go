package server

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// await-idle's signals (docs/superpowers/specs/2026-11-08-await-idle-signal-design.md).
// The quiet leg is byte quiescence, as await-idle always was. The lifecycle
// and program legs fire when their state leaves `working`, and legSatisfied is
// the one rule that decides whether such a state belongs to the turn the
// waiter cares about.

// LifecycleReading is what the lifecycle leg reads from the task store.
type LifecycleReading struct {
	State         protocol.LifecycleState
	At, WorkingAt int64
}

// IdleWatchSpec is what one armed watcher evaluates.
type IdleWatchSpec struct {
	Signal    protocol.AwaitIdleSignal
	Threshold time.Duration
	// Lifecycle reads the task's lifecycle; required when Signal is
	// Lifecycle or Any, nil otherwise.
	Lifecycle func() LifecycleReading
}

// IdleFire is a watcher's outcome. The readings are taken at decision time
// whatever the signal, so a waiter can tell a turn end from a permission
// prompt without a snapshot (spec D8).
type IdleFire struct {
	Stopped      bool
	LastOutput   int64
	FiredBy      protocol.AwaitIdleSignal
	Lifecycle    protocol.LifecycleState
	ProgramState protocol.ProgramState
	ProgramKind  protocol.ProgramKind
}

// idleStamps are the server-clock stamps legSatisfied compares.
type idleStamps struct {
	Now, MuxStart, LastInput, LastOutput int64
	Threshold                            time.Duration
}

// legSatisfied decides one lifecycle or program leg (spec D6, D18). A
// non-working state counts if either:
//
//	(a) it was reported after the later of the last input and the session's
//	    start, and — when there was input — a `working` followed that input.
//	    The `working` shuts out the previous turn's late async Stop hook, which
//	    the server stamps on receipt and so can land after the input;
//	(b) since the last input the session has produced no output for the
//	    threshold: the input started nothing (text without Enter, cursor
//	    keys), so no new state will be reported and quiescence decides.
func legSatisfied(nonWorking bool, stateAt, workingAt int64, s idleStamps) bool {
	if !nonWorking {
		return false
	}
	if reportCounts(stateAt, workingAt, max(s.LastInput, s.MuxStart), s.MuxStart, s.LastInput, false) {
		return true
	}
	return s.LastOutput != 0 && s.Now-max(s.LastInput, s.LastOutput) >= s.Threshold.Nanoseconds()
}

// reportCounts is the one rule await-idle (legSatisfied) and the background
// run evaluator (evaluateRun) share: whether a non-working state reported at
// stateAt counts as the end of a turn. It must have been reported after
// floor; when there was input after base, a `working` (at workingAt) must
// have followed that input — which shuts out the previous turn's late async
// Stop hook (await-idle D6); and, with requireWork, a `working` since base
// must have preceded the report (G16: a run that never worked has not ended
// a turn).
func reportCounts(stateAt, workingAt, floor, base, lastInput int64, requireWork bool) bool {
	if stateAt <= floor {
		return false
	}
	if lastInput > base && workingAt <= lastInput {
		return false
	}
	if requireWork && (workingAt == 0 || workingAt < base || workingAt >= stateAt) {
		return false
	}
	return true
}

func lifecycleNonWorking(st protocol.LifecycleState) bool {
	switch st {
	case protocol.LifecycleState_Waiting, protocol.LifecycleState_Idle, protocol.LifecycleState_Error:
		return true
	}
	return false
}

func programNonWorking(st protocol.ProgramState) bool {
	switch st {
	case protocol.ProgramState_Idle, protocol.ProgramState_Done, protocol.ProgramState_Blocked, protocol.ProgramState_Error:
		return true
	}
	return false
}

func validAwaitIdleSignal(s protocol.AwaitIdleSignal) bool {
	switch s {
	case protocol.AwaitIdleSignal_Quiet, protocol.AwaitIdleSignal_Lifecycle,
		protocol.AwaitIdleSignal_Program, protocol.AwaitIdleSignal_Any:
		return true
	}
	return false
}

// evaluateIdle runs one tick of a watcher: the legs its signal selects, in the
// order program → lifecycle → quiet so the most specific reading wins a tick
// where several hold (spec D4). fired reports whether a leg was satisfied.
// Each source is read ONCE per tick, and the fire reports exactly what the
// decision saw (final review I1).
func (m *SessionMux) evaluateIdle(spec IdleWatchSpec, now int64) (IdleFire, bool) {
	fire, pat, pwat, lc := m.idleSnapshot(spec)
	lo := fire.LastOutput
	s := idleStamps{Now: now, MuxStart: m.MuxStartUnixNano(), LastInput: m.LastInputUnixNano(), LastOutput: lo, Threshold: spec.Threshold}
	sig := spec.Signal
	if (sig == protocol.AwaitIdleSignal_Program || sig == protocol.AwaitIdleSignal_Any) &&
		legSatisfied(programNonWorking(fire.ProgramState), pat, pwat, s) {
		fire.FiredBy = protocol.AwaitIdleSignal_Program
		return fire, true
	}
	if (sig == protocol.AwaitIdleSignal_Lifecycle || sig == protocol.AwaitIdleSignal_Any) &&
		legSatisfied(lifecycleNonWorking(lc.State), lc.At, lc.WorkingAt, s) {
		fire.FiredBy = protocol.AwaitIdleSignal_Lifecycle
		return fire, true
	}
	if (sig == protocol.AwaitIdleSignal_Quiet || sig == protocol.AwaitIdleSignal_Any) &&
		lo != 0 && now-lo >= spec.Threshold.Nanoseconds() {
		fire.FiredBy = protocol.AwaitIdleSignal_Quiet
		return fire, true
	}
	return fire, false
}

// idleSnapshot reads every source once: the program state with its stamps
// under one lock, the lifecycle reading in one call.
func (m *SessionMux) idleSnapshot(spec IdleWatchSpec) (f IdleFire, programAt, programWorkingAt int64, lc LifecycleReading) {
	pst, pkind, pat, pwat := m.programSnapshot()
	if spec.Lifecycle != nil {
		lc = spec.Lifecycle()
	}
	f = IdleFire{LastOutput: m.LastOutputUnixNano(), ProgramState: pst, ProgramKind: pkind, Lifecycle: lc.State}
	return f, pat, pwat, lc
}

// idleReadings is an IdleFire with the current readings and no leg chosen.
func (m *SessionMux) idleReadings(spec IdleWatchSpec) IdleFire {
	f, _, _, _ := m.idleSnapshot(spec)
	return f
}
