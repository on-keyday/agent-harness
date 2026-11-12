package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func TestStartQueuedSessionSendsPromptStreamAndDetachedMux(t *testing.T) {
	h := newTestHandler(t)
	st := newFakeStream(t)
	rc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9140-1"), nextBidi: st, autoSendStreams: true}
	now := time.Now()
	h.Registry.Add(&RunnerEntry{
		ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: rc,
	})
	id, err := h.Tasks.CreateWithinLimits("/r", "triage new issues", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "claude",
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour, Rows: 40, Cols: 120}})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Registry.BindTask(tcid("R"), id) {
		t.Fatal("bind")
	}
	h.Tasks.Assign(id, testRunnerID("R"), "", false)

	if err := h.StartQueuedSession(tcid("R"), id); err != nil {
		t.Fatalf("StartQueuedSession: %v", err)
	}

	if len(rc.sent) != 1 || rc.sent[0][0] != byte(appwire.AppKind_RunnerControl) {
		t.Fatalf("runner conn sent %d messages", len(rc.sent))
	}
	rr := &protocol.RunnerRequest{}
	if _, err := rr.Decode(rc.sent[0][1:]); err != nil || rr.Kind != protocol.RunnerRequestType_OpenExec {
		t.Fatalf("decode: %v kind %v", err, rr.Kind)
	}
	oer := rr.OpenExec()
	if !oer.PromptPresent() || oer.PromptStreamId() == nil || len(rc.sendStreams) != 1 ||
		*oer.PromptStreamId() != uint64(rc.sendStreams[0].streamID) || string(oer.AgentProfile) != "claude" {
		t.Fatalf("OpenExec prompt present %v id %v, %d send streams, profile %q",
			oer.PromptPresent(), oer.PromptStreamId(), len(rc.sendStreams), oer.AgentProfile)
	}
	ps := rc.sendStreams[0]
	var body protocol.PromptBody
	if !ps.eofSent || body.DecodeExact(ps.bytes) != nil || string(body.Prompt) != "triage new issues" {
		t.Fatalf("prompt stream eof %v body %q", ps.eofSent, body.Prompt)
	}

	mux := h.Sessions.Get(id)
	if mux == nil {
		t.Fatal("no session mux registered")
	}
	if cols, rows := mux.screenSize(); cols != 120 || rows != 40 {
		t.Fatalf("mux size %dx%d, want 120 cols x 40 rows", cols, rows)
	}
	if e, _ := h.Tasks.Get(id); e.Status != protocol.TaskStatus_Detached {
		t.Fatalf("status %v, want Detached", e.Status)
	}
}
