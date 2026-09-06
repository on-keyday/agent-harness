package server

import (
	"context"
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func trsfRequest(t *testing.T, target protocol.TrsfTarget, runner protocol.RunnerID) []byte {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_TrsfState, RequestId: 21}
	req.SetTrsfState(protocol.TrsfStateRequest{Target: target, RunnerCid: runner})
	return encodeTaskControlRequest(t, req)
}

func trsfCaller(t *testing.T, h *TaskHandler, port string) *fakeConn {
	t.Helper()
	idHex := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + port + "-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, idHex)
	return conn
}

// A runner's connection multiplexes every task on it, so its congestion
// counters are the sum over all of them and cannot be attributed to one. That
// is why the runner branch needs the global view rather than a filter -- and
// why holding every capability does not substitute for it. The refusal must not
// depend on a bit: runner_admin in particular was deliberately NOT reused,
// because it also carries the power to make the server dial a new runner.
func TestRunnerTrsfStateNeedsTheGlobalViewNotACapability(t *testing.T) {
	h := newTestHandler(t)
	asked := false
	h.RunnerTrsfStateFn = func(context.Context, protocol.RunnerID) ([]protocol.TrsfConnState, error) {
		asked = true
		return nil, nil
	}
	conn := trsfCaller(t, h, "9801") // Capability_All, but a confined scope

	h.Handle(conn, trsfRequest(t, protocol.TrsfTarget_Runner, protocol.RunnerID{}))

	if asked {
		t.Fatal("a confined caller reached a runner's transport state")
	}
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_TrsfState {
		t.Fatalf("resp.Kind = %v", resp.Kind)
	}
	if got := resp.TrsfState(); got == nil || got.Status != protocol.TrsfStateStatus_NotPermitted {
		t.Fatalf("status = %+v, want not_permitted", got)
	}
}

// The server view reads no capability either. What bounds it is the visibility
// projection, handed in as (allowed, globalView) -- the same pair connInfoFor
// takes -- so a confined caller gets the rows for connections it can already
// see listed, and no others.
func TestServerTrsfStateReadsNoCapabilityAndPassesTheVisibility(t *testing.T) {
	h := newTestHandler(t)
	var sawGlobal bool
	var sawAllowed map[string]bool
	h.TrsfStateFn = func(allowed map[string]bool, globalView bool) []protocol.TrsfConnState {
		sawGlobal, sawAllowed = globalView, allowed
		return []protocol.TrsfConnState{{Cwnd: 4242}}
	}
	conn := trsfCaller(t, h, "9802")

	h.Handle(conn, trsfRequest(t, protocol.TrsfTarget_Server, protocol.RunnerID{}))

	if sawGlobal {
		t.Error("a confined caller was handed the global view")
	}
	if sawAllowed == nil {
		t.Error("no visible set was passed; the filter would be a no-op")
	}
	resp := lastTaskControlResponse(t, conn)
	r := resp.TrsfState()
	if r == nil || r.Status != protocol.TrsfStateStatus_Ok {
		t.Fatalf("status = %+v", r)
	}
	if len(r.Conns) != 1 || r.Conns[0].Cwnd != 4242 {
		t.Fatalf("rows did not survive: %+v", r.Conns)
	}
	if r.Count != 1 {
		t.Errorf("Count = %d, want 1 -- a count that disagrees with the rows is undecodable", r.Count)
	}
}

// "No such runner" and "the runner did not answer" send the operator to
// different places: one is a stale id, the other is a runner worth looking at.
func TestRunnerTrsfStateSeparatesOfflineFromSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want protocol.TrsfStateStatus
	}{
		{"offline", errRunnerOffline, protocol.TrsfStateStatus_RunnerOffline},
		{"silent", errors.New("deadline exceeded"), protocol.TrsfStateStatus_Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t)
			h.RunnerTrsfStateFn = func(context.Context, protocol.RunnerID) ([]protocol.TrsfConnState, error) {
				return nil, tc.err
			}
			// Operator: no principal, so the global view is granted.
			conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9803-1")}
			h.Handle(conn, trsfRequest(t, protocol.TrsfTarget_Runner, protocol.RunnerID{}))
			resp := lastTaskControlResponse(t, conn)
			got := resp.TrsfState()
			if got == nil || got.Status != tc.want {
				t.Fatalf("status = %+v, want %v", got, tc.want)
			}
		})
	}
}
