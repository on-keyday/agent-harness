package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// openAttached sends an attached (detached = 0) fresh or resuming open.
func (f *resumeFixture) openAttached(t *testing.T, resume string) protocol.TaskControlResponse {
	t.Helper()
	f.reqID++
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	if resume != "" {
		oi.ResumeTaskId = hexToTaskID(t, resume)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: f.reqID}
	req.SetOpenInteractive(oi)
	f.h.Handle(f.conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, f.conn)
}

func TestAttachedOpenNeedsExecControl(t *testing.T) {
	f := newResumeFixture(t, protocol.Capability_Spawn, authority.Scope{Base: protocol.ScopeBase_Subtree})
	resp := f.openAttached(t, "")
	if resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("spawn-only attached open: kind %v, want PermissionDenied", resp.Kind)
	}
	if got := resp.PermissionDenied().RequiredCap; got != protocol.Capability_ExecControl {
		t.Fatalf("RequiredCap = %s, want exec_control", authority.CapsLabel(got))
	}
	if resp := f.open(t, "", false); resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Fatal("spawn-only detached open was denied")
	}
}

func TestAttachedOpenOutOfControlScope(t *testing.T) {
	noControl := []authority.ScopeOverride{{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_None, ExcludeSelf: true}}

	f := newResumeFixture(t, protocol.Capability_Spawn|protocol.Capability_ExecControl,
		authority.Scope{Base: protocol.ScopeBase_Subtree, Overrides: noControl})
	r1 := f.openAttached(t, "")
	if o := r1.OpenInteractive(); o == nil || o.Status != protocol.OpenInteractiveStatus_DetachRequired {
		t.Fatalf("fresh attached open with exec_control scoped to nothing: %+v, want detach_required", o)
	}

	f = newResumeFixture(t, protocol.Capability_Resume|protocol.Capability_ExecControl,
		authority.Scope{Base: protocol.ScopeBase_Global, Overrides: noControl})
	target := f.terminalTask(t, false)
	r2 := f.openAttached(t, target)
	if o := r2.OpenInteractive(); o == nil || o.Status != protocol.OpenInteractiveStatus_DetachRequired {
		t.Fatalf("resume attached open outside exec_control's scope: %+v, want detach_required", o)
	}
}

func TestScopeCoversNewChildAgreesWithActionSet(t *testing.T) {
	for _, c := range []struct {
		name  string
		scope authority.Scope
	}{
		{"subtree", authority.Scope{Base: protocol.ScopeBase_Subtree}},
		{"descendants", authority.Scope{Base: protocol.ScopeBase_Subtree, ExcludeSelf: true}},
		{"global", authority.Scope{Base: protocol.ScopeBase_Global}},
		{"none", authority.Scope{Base: protocol.ScopeBase_None}},
		{"ids", authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{"ee000000000000000000000000000000"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHandler(t)
			self := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
				protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_ExecControl, c.scope, "")
			child := h.Tasks.Create("/r", "c", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
				hexToTaskID(t, self), "", protocol.RunnerSelector{}, nil, protocol.Capability_None, authority.Scope{}, "")
			all, allowed := authority.ActionSet(self, c.scope, protocol.Capability_ExecControl, childIndexOf(h.Tasks))
			want := all || allowed[child]
			if got := authority.CoversNewChild(c.scope, protocol.Capability_ExecControl); got != want {
				t.Fatalf("authority.CoversNewChild = %v, authority.ActionSet says %v", got, want)
			}
		})
	}
}

// openRunner registers a runner on /r whose conn hands out a blocking stream,
// so the handler's teardown does not race the test (the open-interactive flake
// family). It returns that stream.
func openRunner(t *testing.T, h *TaskHandler) *fakeBidiStream {
	t.Helper()
	st := newFakeStream(t)
	rc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9130-1"), nextBidi: st}
	now := time.Now()
	h.Registry.Add(&RunnerEntry{
		ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: rc,
	})
	return st
}

// Review Focus 1: an operator connection has no principal and must keep
// opening attached sessions.
func TestOperatorAttachedOpenStillWorks(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9131-1"), nextBidi: newFakeStream(t)}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 1}
	req.SetOpenInteractive(oi)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	r := lastTaskControlResponse(t, conn)
	o := r.OpenInteractive()
	if o == nil || o.Status != protocol.OpenInteractiveStatus_Ok || !o.Attached() {
		t.Fatalf("operator attached open: %+v, want ok and attached", o)
	}
}

// The first bidirectional stream the server opens toward a client is id 0.
func TestAttachedOpenFirstStreamIdZero(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	tui := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9132-1"), nextBidi: newFakeStream(t)} // id 0
	oi := &protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	resp, _ := h.handleOpenInteractive("", tui, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if id := resp.StreamId(); resp.Status != protocol.OpenInteractiveStatus_Ok || !resp.Attached() || id == nil || *id != 0 {
		t.Fatalf("status %v attached %v stream %v, want ok, attached, stream 0", resp.Status, resp.Attached(), id)
	}
}

func detachedOpen(rows, cols uint16) *protocol.OpenInteractiveRequest {
	oi := &protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	oi.SetDetached(true)
	oi.SetInitialRows(rows)
	oi.SetInitialCols(cols)
	return oi
}

func TestDetachedOpenNoClientStream(t *testing.T) {
	h := newTestHandler(t)
	runner := openRunner(t, h)
	tui := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9133-1"), nextStreamID: 7}
	resp, _ := h.handleOpenInteractive("", tui, detachedOpen(0, 0), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_Ok || resp.Attached() || resp.StreamId() != nil {
		t.Fatalf("status %v attached %v stream %v, want ok with no stream", resp.Status, resp.Attached(), resp.StreamId())
	}
	if len(tui.bidiStreams) != 0 {
		t.Fatalf("a client stream was created on a detached open")
	}
	e, _ := h.Tasks.Get(hex.EncodeToString(resp.TaskId.Id[:]))
	if e.Status != protocol.TaskStatus_Detached {
		t.Fatalf("task status %v, want Detached", e.Status)
	}
	if runner.IsClosed() {
		t.Fatal("the runner stream was torn down")
	}
}

func TestDetachedOpenAppliesInitialSize(t *testing.T) {
	h := newTestHandler(t)
	runner := openRunner(t, h)
	resp, _ := h.handleOpenInteractive("", nil, detachedOpen(40, 150), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v", resp.Status)
	}
	cols, rows, ok := winSizeOf(runner.WaitWritten(t, frameHeaderSize+1))
	if !ok || cols != 150 || rows != 40 {
		t.Fatalf("runner got size %dx%d ok=%v, want 150x40", cols, rows, ok)
	}

	// Review Focus 3: one zero value means no size, as applyInitialWindowSize.
	h2 := newTestHandler(t)
	runner2 := openRunner(t, h2)
	if resp, _ := h2.handleOpenInteractive("", nil, detachedOpen(40, 0), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All); resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v", resp.Status)
	}
	time.Sleep(50 * time.Millisecond)
	if w := runner2.Written(); len(w) != 0 {
		t.Fatalf("a half-given size wrote %d bytes to the runner", len(w))
	}
}

func TestDetachedX11Refused(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	oi := detachedOpen(0, 0)
	oi.SetX11Enabled(true)
	oi.SetX11(protocol.X11Forward{Display: 10})
	resp, _ := h.handleOpenInteractive("", nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_InternalError {
		t.Fatalf("status %v, want internal_error", resp.Status)
	}
}
