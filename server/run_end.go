package server

import "github.com/on-keyday/agent-harness/runner/protocol"

// runInputs is everything evaluateRun reads, gathered from the task record
// and the session's mux at one instant (unix nanos throughout).
type runInputs struct {
	Now, RunStart, SwitchedAt, MuxStart, LastInput, LastOutput int64
	Kind                                                       protocol.TaskKind
	Cond                                                       RunConditions // after WithDefaults
	Life                                                       LifecycleReading
	Prog                                                       protocol.ProgramState
	ProgAt, ProgWorkingAt                                      int64
	// SawOutputThisRun: output arrived since the run started — on this mux,
	// or before a deliberate restart (carried, A-D13).
	SawOutputThisRun bool
}

// evaluateRun decides whether a background run ends now, and why (pillar A
// spec A-D8, A-D9). Unlike await-idle's evaluateIdle, waiting/blocked never
// end a turn — they run the blocked clock — and error is its own end. The
// shared part, whether a report ends a turn, is reportCounts, here with
// requireWork (G16) counted from the run's start and a floor at the later of
// the run start, a mode switch and the last input.
func evaluateRun(in runInputs) (protocol.EndReason, bool) {
	since := max(in.RunStart, in.SwitchedAt)
	stream := in.Kind == protocol.TaskKind_Stream

	if in.Life.State == protocol.LifecycleState_Error && in.Life.At > since {
		return protocol.EndReason_AgentError, true
	}
	if !stream && in.Prog == protocol.ProgramState_Error && in.ProgAt > since {
		return protocol.EndReason_AgentError, true
	}

	// The blocked clock: a blocked report from this run, counted from the
	// later of the report and a switch. A state left from a previous run is
	// not this run's.
	blockedFor := func(at int64) bool {
		return in.Cond.Blocked > 0 && at >= in.RunStart && in.Now-max(at, in.SwitchedAt) >= int64(in.Cond.Blocked)
	}
	if in.Life.State == protocol.LifecycleState_Waiting && blockedFor(in.Life.At) {
		return protocol.EndReason_BlockedTimeout, true
	}
	if !stream && in.Prog == protocol.ProgramState_Blocked && blockedFor(in.ProgAt) {
		return protocol.EndReason_BlockedTimeout, true
	}

	sig := in.Cond.TurnEnd
	if sig == protocol.TurnEndSignal_Off {
		return protocol.EndReason_None, false
	}
	floor := max(in.RunStart, in.SwitchedAt, in.LastInput)
	leg := func(s protocol.TurnEndSignal) bool { return sig == s || sig == protocol.TurnEndSignal_Any }
	if !stream && leg(protocol.TurnEndSignal_Program) &&
		(in.Prog == protocol.ProgramState_Idle || in.Prog == protocol.ProgramState_Done) &&
		reportCounts(in.ProgAt, in.ProgWorkingAt, floor, in.RunStart, in.LastInput, true) {
		return protocol.EndReason_TurnDoneProgram, true
	}
	if leg(protocol.TurnEndSignal_Lifecycle) && in.Life.State == protocol.LifecycleState_Idle &&
		reportCounts(in.Life.At, in.Life.WorkingAt, floor, in.RunStart, in.LastInput, true) {
		return protocol.EndReason_TurnDoneLifecycle, true
	}
	if !stream && leg(protocol.TurnEndSignal_Quiet) && in.SawOutputThisRun && in.Cond.Quiet > 0 &&
		in.Now-max(in.LastOutput, in.LastInput, in.SwitchedAt, in.MuxStart) >= int64(in.Cond.Quiet) {
		return protocol.EndReason_TurnDoneQuiet, true
	}
	return protocol.EndReason_None, false
}

// endBackgroundRun ends a background run for an automatic reason (A-D8): a
// cancel recording why, through the same path a human's cancel takes. The
// cancel also wakes Tick, since the run's slot frees when the runner answers.
//
// The cancel is conditional (TaskStore.CancelBackgroundRun): a fire computed
// before a switch to foreground, or for a run since replaced, cancels nothing.
func (h *TaskHandler) endBackgroundRun(hit DeadlineHit) {
	if !h.Tasks.CancelBackgroundRun(hit.ID, hit.Reason, hit.SwitchedAt) {
		return
	}
	if h.OnChange != nil {
		h.OnChange()
	}
}
