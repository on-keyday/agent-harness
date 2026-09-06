package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/peer"
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
	h.RunnerTrsfStateFn = func(context.Context, protocol.RunnerID) ([]protocol.TrsfConnState, int64, error) {
		asked = true
		return nil, 0, nil
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
	h.TrsfStateFn = func(allowed map[string]bool, globalView bool) ([]protocol.TrsfConnState, int64) {
		sawGlobal, sawAllowed = globalView, allowed
		return []protocol.TrsfConnState{{Cwnd: 4242}}, time.Now().UnixNano()
	}
	conn := trsfCaller(t, h, "9802")
	conn.nextSendStreamID = 7 // the rows travel on a stream, so one must exist

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
	// The rows are on a stream; the response carries only its id.
	if r.StreamId != 7 {
		t.Errorf("StreamId = %d, want the stream the conn handed out", r.StreamId)
	}
	// And the row really is on it, rather than lost between the two.
	var body protocol.TrsfStateResultBody
	if err := body.DecodeExact(conn.sendStreamBytes(t, 7)); err != nil {
		t.Fatalf("decode body off the stream: %v", err)
	}
	if len(body.Conns) != 1 || body.Conns[0].Cwnd != 4242 {
		t.Fatalf("rows did not survive the stream: %+v", body.Conns)
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
			h.RunnerTrsfStateFn = func(context.Context, protocol.RunnerID) ([]protocol.TrsfConnState, int64, error) {
				return nil, 0, tc.err
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

// The response must fit a path MTU whatever the server is carrying, because
// objproto does not split an application message and a failed send is a
// dropped error the caller experiences as a hang.
//
// This is the guard for a bug that shipped: the rows were inline, ten of them
// came to 1229 bytes against udp's 1200, and every local test passed because
// loopback's MTU is 65536. The response is now constant-size by construction,
// and this pins that.
func TestTrsfResponseFitsAnyPathMTU(t *testing.T) {
	udp, _ := peer.MTUForTransport("udp")
	var big int
	for _, streamID := range []uint64{0, 1, ^uint64(0)} {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_TrsfState, RequestId: ^uint32(0)}
		resp.SetTrsfState(protocol.TrsfStateResponse{
			Status: protocol.TrsfStateStatus_Ok, StreamId: streamID,
		})
		n := len(resp.MustAppend([]byte{0}))
		if n > big {
			big = n
		}
	}
	if big >= udp {
		t.Fatalf("a trsf_state response is %d bytes against a udp path MTU of %d: "+
			"it cannot cross a real path, and the send error is dropped", big, udp)
	}
	// And the size must not depend on how much the server is carrying. A row
	// count that changes it is a row list creeping back into the response.
	if big > 64 {
		t.Errorf("the response is %d bytes; it should be a status and a stream id, "+
			"so anything this large means payload is riding along", big)
	}
}
