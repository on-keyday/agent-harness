package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestEvaluateRun(t *testing.T) {
	base := func() runInputs {
		return runInputs{
			Now: ms(10_000), RunStart: ms(100), MuxStart: ms(100),
			Kind: protocol.TaskKind_Interactive,
			Cond: RunConditions{Quiet: 2 * time.Second, Blocked: 3 * time.Second, TurnEnd: protocol.TurnEndSignal_Any},
		}
	}
	type row struct {
		name   string
		mod    func(*runInputs)
		want   protocol.EndReason
		wantOK bool
	}
	rows := []row{
		{"nothing reported, no output: runs on", func(in *runInputs) {}, 0, false},
		{"lifecycle idle after working ends the turn", func(in *runInputs) {
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(900), WorkingAt: ms(500)}
			in.LastOutput = ms(9_900)
		}, protocol.EndReason_TurnDoneLifecycle, true},
		{"lifecycle idle with no working since the run started does not", func(in *runInputs) {
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(900), WorkingAt: ms(50)}
			in.LastOutput = ms(9_900)
		}, 0, false},
		{"after input, idle needs a working after the input", func(in *runInputs) {
			in.LastInput = ms(1000)
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(1200), WorkingAt: ms(500)}
			in.LastOutput = ms(9_900)
		}, 0, false},
		{"after input, working then idle ends it", func(in *runInputs) {
			in.LastInput = ms(1000)
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(1300), WorkingAt: ms(1100)}
			in.LastOutput = ms(9_900)
		}, protocol.EndReason_TurnDoneLifecycle, true},
		{"waiting never ends a turn; within the blocked limit runs on", func(in *runInputs) {
			in.Life = LifecycleReading{State: protocol.LifecycleState_Waiting, At: ms(8_000), WorkingAt: ms(500)}
			in.LastOutput = ms(9_900)
		}, 0, false},
		{"waiting past the blocked limit", func(in *runInputs) {
			in.Life = LifecycleReading{State: protocol.LifecycleState_Waiting, At: ms(6_000), WorkingAt: ms(500)}
			in.LastOutput = ms(9_900)
		}, protocol.EndReason_BlockedTimeout, true},
		{"program blocked past the limit", func(in *runInputs) {
			in.Prog, in.ProgAt, in.ProgWorkingAt = protocol.ProgramState_Blocked, ms(6_000), ms(500)
			in.LastOutput = ms(9_900)
		}, protocol.EndReason_BlockedTimeout, true},
		{"program done after working", func(in *runInputs) {
			in.Prog, in.ProgAt, in.ProgWorkingAt = protocol.ProgramState_Done, ms(900), ms(500)
			in.LastOutput = ms(9_900)
		}, protocol.EndReason_TurnDoneProgram, true},
		{"a turn that ended before the switch does not end the run", func(in *runInputs) {
			in.SwitchedAt = ms(2000)
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(1500), WorkingAt: ms(500)}
			in.LastOutput = ms(9_900)
		}, 0, false},
		{"a turn that ends after the switch does", func(in *runInputs) {
			in.SwitchedAt = ms(2000)
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(2500), WorkingAt: ms(2200)}
			in.LastOutput = ms(9_900)
		}, protocol.EndReason_TurnDoneLifecycle, true},
		{"quiet after output this run", func(in *runInputs) {
			in.SawOutputThisRun, in.LastOutput = true, ms(7_000)
		}, protocol.EndReason_TurnDoneQuiet, true},
		{"quiet with no output this run does not fire", func(in *runInputs) {
			in.LastOutput = 0
		}, 0, false},
		{"quiet counts from the switch too", func(in *runInputs) {
			in.SawOutputThisRun, in.LastOutput, in.SwitchedAt = true, ms(3_000), ms(9_000)
		}, 0, false},
		{"the stream kind ignores quiet", func(in *runInputs) {
			in.Kind = protocol.TaskKind_Stream
			in.SawOutputThisRun, in.LastOutput = true, ms(1_000)
		}, 0, false},
		{"turn end off: idle does not end it", func(in *runInputs) {
			in.Cond.TurnEnd = protocol.TurnEndSignal_Off
			in.Life = LifecycleReading{State: protocol.LifecycleState_Idle, At: ms(900), WorkingAt: ms(500)}
			in.SawOutputThisRun, in.LastOutput = true, ms(1_000)
		}, 0, false},
		{"turn end off: blocked still ends it", func(in *runInputs) {
			in.Cond.TurnEnd = protocol.TurnEndSignal_Off
			in.Life = LifecycleReading{State: protocol.LifecycleState_Waiting, At: ms(6_000), WorkingAt: ms(500)}
		}, protocol.EndReason_BlockedTimeout, true},
		{"turn end lifecycle only: program done does not end it", func(in *runInputs) {
			in.Cond.TurnEnd = protocol.TurnEndSignal_Lifecycle
			in.Prog, in.ProgAt, in.ProgWorkingAt = protocol.ProgramState_Done, ms(900), ms(500)
			in.SawOutputThisRun, in.LastOutput = true, ms(1_000)
		}, 0, false},
		{"agent error beats a turn end", func(in *runInputs) {
			in.Life = LifecycleReading{State: protocol.LifecycleState_Error, At: ms(900), WorkingAt: ms(500)}
			in.Prog, in.ProgAt, in.ProgWorkingAt = protocol.ProgramState_Done, ms(900), ms(500)
		}, protocol.EndReason_AgentError, true},
		{"an error reported before the run started does not count", func(in *runInputs) {
			in.Life = LifecycleReading{State: protocol.LifecycleState_Error, At: ms(50)}
		}, 0, false},
	}
	for _, r := range rows {
		in := base()
		r.mod(&in)
		got, ok := evaluateRun(in)
		if ok != r.wantOK || (ok && got != r.want) {
			t.Errorf("%s: evaluateRun = %v, %v; want %v, %v", r.name, got, ok, r.want, r.wantOK)
		}
	}
}
