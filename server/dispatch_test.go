package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
)

func TestDispatchRoutesByKind(t *testing.T) {
	var runnerControlCalled bool
	var runnerControlPayload []byte
	var taskControlCalled bool
	var taskControlPayload []byte

	d := Dispatcher{
		OnRunnerControl: func(conn ConnHandle, payload []byte) {
			runnerControlCalled = true
			runnerControlPayload = append([]byte(nil), payload...)
		},
		OnTaskControl: func(conn ConnHandle, payload []byte) {
			taskControlCalled = true
			taskControlPayload = append([]byte(nil), payload...)
		},
	}

	// Test RunnerControl
	msg1 := []byte{byte(appwire.AppKind_RunnerControl), 0x00, 0x01}
	d.Dispatch(nil, msg1)

	if !runnerControlCalled {
		t.Error("expected OnRunnerControl to be called")
	}
	if taskControlCalled {
		t.Error("expected OnTaskControl to NOT be called")
	}
	if len(runnerControlPayload) != 2 || runnerControlPayload[0] != 0x00 || runnerControlPayload[1] != 0x01 {
		t.Errorf("expected payload [0x00, 0x01], got %v", runnerControlPayload)
	}

	// Reset state
	runnerControlCalled = false
	runnerControlPayload = nil
	taskControlCalled = false
	taskControlPayload = nil

	// Test TaskControl
	msg2 := []byte{byte(appwire.AppKind_TaskControl), 0x42}
	d.Dispatch(nil, msg2)

	if runnerControlCalled {
		t.Error("expected OnRunnerControl to NOT be called")
	}
	if !taskControlCalled {
		t.Error("expected OnTaskControl to be called")
	}
	if len(taskControlPayload) != 1 || taskControlPayload[0] != 0x42 {
		t.Errorf("expected payload [0x42], got %v", taskControlPayload)
	}
}

func TestDispatchEmpty(t *testing.T) {
	called := false
	d := Dispatcher{
		OnRunnerControl: func(conn ConnHandle, payload []byte) {
			called = true
		},
		OnTaskControl: func(conn ConnHandle, payload []byte) {
			called = true
		},
	}

	// Test nil message
	d.Dispatch(nil, nil)
	if called {
		t.Error("expected no callback for nil message")
	}

	// Test empty message
	d.Dispatch(nil, []byte{})
	if called {
		t.Error("expected no callback for empty message")
	}
}

func TestDispatchUnknownKind(t *testing.T) {
	var runnerControlCalled bool
	var taskControlCalled bool

	d := Dispatcher{
		OnRunnerControl: func(conn ConnHandle, payload []byte) {
			runnerControlCalled = true
		},
		OnTaskControl: func(conn ConnHandle, payload []byte) {
			taskControlCalled = true
		},
	}

	msg := []byte{0xFF, 0x00}
	d.Dispatch(nil, msg)

	if runnerControlCalled {
		t.Error("expected OnRunnerControl to NOT be called for unknown kind")
	}
	if taskControlCalled {
		t.Error("expected OnTaskControl to NOT be called for unknown kind")
	}
}

func TestDispatchNilCallbacks(t *testing.T) {
	d := Dispatcher{
		OnRunnerControl: nil,
		OnTaskControl:   nil,
	}

	// This should not panic
	msg := []byte{byte(appwire.AppKind_RunnerControl)}
	d.Dispatch(nil, msg)

	// Also test with TaskControl
	msg2 := []byte{byte(appwire.AppKind_TaskControl)}
	d.Dispatch(nil, msg2)
}
