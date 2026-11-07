package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestWatcherWaitsFor_Signal(t *testing.T) {
	for _, c := range []struct {
		sig  protocol.AwaitIdleSignal
		want string
	}{
		{protocol.AwaitIdleSignal_Quiet, "idle quiet 2500ms"},
		{protocol.AwaitIdleSignal_Lifecycle, "idle lifecycle"},
		{protocol.AwaitIdleSignal_Program, "idle program"},
		{protocol.AwaitIdleSignal_Any, "idle any 2500ms"},
	} {
		w := protocol.AwaitIdleWatcherInfo{Condition: protocol.AwaitCondition_Idle, ThresholdMs: 2500, Signal: c.sig}
		if got := watcherWaitsFor(&w); got != c.want {
			t.Errorf("watcherWaitsFor(%v) = %q, want %q", c.sig, got, c.want)
		}
	}
}

func TestAwaitIdleResult_NamesLegAndState(t *testing.T) {
	for _, c := range []struct {
		msg  AwaitIdleResultMsg
		want string
	}{
		{AwaitIdleResultMsg{FiredBy: protocol.AwaitIdleSignal_Program, ProgramState: protocol.ProgramState_Done}, "session is idle (program done)"},
		{AwaitIdleResultMsg{FiredBy: protocol.AwaitIdleSignal_Program, ProgramState: protocol.ProgramState_Blocked, ProgramKind: protocol.ProgramKind_Permission}, "(program blocked:permission)"},
		{AwaitIdleResultMsg{FiredBy: protocol.AwaitIdleSignal_Lifecycle, Lifecycle: protocol.LifecycleState_Waiting}, "(lifecycle waiting)"},
		{AwaitIdleResultMsg{FiredBy: protocol.AwaitIdleSignal_Quiet, LastOutputAt: 1}, "session is idle (last output"},
	} {
		a := New(Config{})
		c.msg.TaskID = "1a2b3c4d00000000000000000000000000"
		c.msg.Status = protocol.AwaitIdleStatus_Fired
		a.Update(c.msg)
		if got := strings.Join(a.cmdresult.lines, "\n"); !strings.Contains(got, c.want) {
			t.Errorf("result for %+v = %q, want it to contain %q", c.msg, got, c.want)
		}
	}
}
