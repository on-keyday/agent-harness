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

func TestDecodeConnStatusEvent(t *testing.T) {
	orig := protocol.ConnStatusEvent{
		Kind: protocol.StatusEventKind_ConnOpened,
		Ts:   9876543,
	}
	orig.Info.SetCid([]byte("testcid"))
	orig.Info.Role = protocol.ConnRole_Cli
	orig.Info.SetRemoteAddr([]byte("127.0.0.1:12345"))
	orig.Info.ConnectedAt = 1234567890
	orig.Info.SetIdentified(false)
	encoded := orig.MustAppend(nil)

	got, err := DecodeConnStatus(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != orig.Kind {
		t.Errorf("Kind got=%v want=%v", got.Kind, orig.Kind)
	}
	if got.Ts != orig.Ts {
		t.Errorf("Ts got=%v want=%v", got.Ts, orig.Ts)
	}
	if string(got.Info.Cid) != string(orig.Info.Cid) {
		t.Errorf("Cid got=%q want=%q", got.Info.Cid, orig.Info.Cid)
	}
	if got.Info.Role != orig.Info.Role {
		t.Errorf("Role got=%v want=%v", got.Info.Role, orig.Info.Role)
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
