package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestDecodeTaskStatusEvent(t *testing.T) {
	orig := protocol.TaskStatusEvent{
		Kind:       protocol.StatusEventKind_TaskQueued,
		Ts:         1234567,
		TaskStatus: protocol.TaskStatus_Queued,
		ExitCode:   0,
	}
	orig.TaskId.Id[0] = 0xAB
	encoded := orig.MustAppend(nil)

	got, err := DecodeTaskStatus(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != orig.Kind {
		t.Errorf("Kind got=%v want=%v", got.Kind, orig.Kind)
	}
	if got.TaskId.Id[0] != 0xAB {
		t.Errorf("TaskId byte: %x", got.TaskId.Id[0])
	}
}

func TestDecodeRunnerStatusEvent(t *testing.T) {
	orig := protocol.RunnerStatusEvent{
		Kind:         protocol.StatusEventKind_RunnerRegistered,
		Ts:           42,
		RunnerStatus: protocol.RunnerStatus_Idle,
	}
	// RunnerID encoder requires IpAddrLen ∈ {4,16}; populate a placeholder.
	orig.RunnerId.SetTransport([]byte("ws"))
	orig.RunnerId.SetIpAddr([]byte{127, 0, 0, 1})
	encoded := orig.MustAppend(nil)

	got, err := DecodeRunnerStatus(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerStatus != orig.RunnerStatus {
		t.Errorf("RunnerStatus got=%v", got.RunnerStatus)
	}
}
