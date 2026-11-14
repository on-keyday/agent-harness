package server

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// transcriptFixture: a task assigned to a registered runner, and a caller
// connection that is either the operator (no principal) or an agent task with
// the given grant.
type transcriptFixture struct {
	h        *TaskHandler
	runner   *fakeConn
	taskHex  string
	callerID int
}

func newTranscriptServerFixture(t *testing.T, runnerServes bool) *transcriptFixture {
	t.Helper()
	h := newTestHandler(t)
	rc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9140-1"), nextStreamID: 100}
	now := time.Now()
	h.Registry.Add(&RunnerEntry{
		ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: rc,
		Transcripts: runnerServes,
	})
	id := h.Tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
	h.Tasks.Assign(id, testRunnerID("R"), "/r/.harness-worktrees/"+id, false)
	return &transcriptFixture{h: h, runner: rc, taskHex: id}
}

// agentCaller wires a caller connection whose principal is a new task holding
// caps over scope.
func (f *transcriptFixture) agentCaller(t *testing.T, caps protocol.Capability, scope authority.Scope) *fakeConn {
	t.Helper()
	self := f.h.Tasks.Create("/r", "me", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: caps, Scope: scope}, "")
	f.callerID++
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9150-" + string(rune('0'+f.callerID))), nextStreamID: 100}
	if f.h.principals == nil {
		f.h.principals = make(map[string]protocol.TaskID)
	}
	f.h.principals[conn.ConnectionID().String()] = hexToTaskID(t, self)
	return conn
}

func (f *transcriptFixture) operator(t *testing.T) *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9160-1"), nextStreamID: 100}
}

func (f *transcriptFixture) list(t *testing.T, conn *fakeConn) protocol.TaskControlResponse {
	t.Helper()
	var body protocol.TranscriptListRequest
	body.TaskId = hexToTaskID(t, f.taskHex)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TranscriptList, RequestId: 3}}
	req.SetTranscriptList(body)
	// The fakes hand out one stream per arming; each request needs its own.
	f.runner.nextStreamID, conn.nextStreamID = 100, 200
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, conn)
}

func transcriptStatus(t *testing.T, r protocol.TaskControlResponse) protocol.TranscriptStatus {
	t.Helper()
	tl := r.TranscriptList()
	if tl == nil {
		t.Fatalf("response kind %v, want a transcript_list answer", r.Kind)
	}
	return tl.Status
}

func (f *transcriptFixture) sentTranscriptRequests(t *testing.T) []protocol.RunnerTranscriptRequest {
	t.Helper()
	var out []protocol.RunnerTranscriptRequest
	for _, rr := range runnerRequests(t, f.runner) {
		if tr := rr.Transcript(); rr.Kind == protocol.RunnerRequestType_Transcript && tr != nil {
			out = append(out, *tr)
		}
	}
	return out
}

func TestTranscriptKindsRequireTranscriptRead(t *testing.T) {
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_TranscriptList, protocol.TaskControlKind_TranscriptGet} {
		if got, ok := requiredCap[k]; !ok || got != protocol.Capability_TranscriptRead {
			t.Fatalf("%v requires %v (gated %v), want transcript_read", k, got, ok)
		}
	}
}

// B-D10: exec_view shows screens; it does not read transcripts.
func TestTranscriptNeedsTranscriptRead(t *testing.T) {
	f := newTranscriptServerFixture(t, true)
	conn := f.agentCaller(t, protocol.Capability_ExecView, authority.Scope{Base: protocol.ScopeBase_Global})
	if r := f.list(t, conn); r.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("exec_view alone: %v, want PermissionDenied", r.Kind)
	}
	if n := len(f.sentTranscriptRequests(t)); n != 0 {
		t.Fatalf("%d requests reached the runner", n)
	}
}

func TestTranscriptOutOfScopeIsNoSuchTask(t *testing.T) {
	f := newTranscriptServerFixture(t, true)
	conn := f.agentCaller(t, protocol.Capability_TranscriptRead, authority.Scope{Base: protocol.ScopeBase_None})
	if st := transcriptStatus(t, f.list(t, conn)); st != protocol.TranscriptStatus_NoSuchTask {
		t.Fatalf("out of scope: %v, want no_such_task", st)
	}
	if n := len(f.sentTranscriptRequests(t)); n != 0 {
		t.Fatalf("%d requests reached the runner", n)
	}
}

// include_shared tells the runner the caller may also read sessions recorded
// in a shared directory: it needs exec_run within scope as well (B-D2).
func TestTranscriptIncludeSharedNeedsExecRun(t *testing.T) {
	f := newTranscriptServerFixture(t, true)
	global := authority.Scope{Base: protocol.ScopeBase_Global}
	without := f.agentCaller(t, protocol.Capability_TranscriptRead, global)
	with := f.agentCaller(t, protocol.Capability_TranscriptRead|protocol.Capability_ExecRun, global)
	if st := transcriptStatus(t, f.list(t, without)); st != protocol.TranscriptStatus_Ok {
		t.Fatalf("status %v", st)
	}
	if st := transcriptStatus(t, f.list(t, with)); st != protocol.TranscriptStatus_Ok {
		t.Fatalf("status %v", st)
	}
	sent := f.sentTranscriptRequests(t)
	if len(sent) != 2 || sent[0].IncludeShared() || !sent[1].IncludeShared() {
		t.Fatalf("include_shared sent: %+v, want false then true", sent)
	}
}

func TestTranscriptRunnerOfflineWhenUnassigned(t *testing.T) {
	f := newTranscriptServerFixture(t, true)
	other := f.h.Tasks.Create("/r", "q", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
	f.taskHex = other // queued: no assigned runner
	if st := transcriptStatus(t, f.list(t, f.operator(t))); st != protocol.TranscriptStatus_RunnerOffline {
		t.Fatalf("unassigned: %v, want runner_offline", st)
	}
}

// A runner that predates transcripts is never sent the request: an unknown
// request type would have the server drop its connection (review of Task 1, I3).
func TestTranscriptRunnerUnsupportedWithoutHelloBit(t *testing.T) {
	f := newTranscriptServerFixture(t, false)
	if st := transcriptStatus(t, f.list(t, f.operator(t))); st != protocol.TranscriptStatus_RunnerUnsupported {
		t.Fatalf("old runner: %v, want runner_unsupported", st)
	}
	if n := len(f.sentTranscriptRequests(t)); n != 0 {
		t.Fatalf("%d requests reached an old runner", n)
	}
}

// The registry learns the bit from the runner's hello.
func TestRunnerEntryRecordsTranscriptsFromHello(t *testing.T) {
	var hello protocol.RunnerHello
	hello.SetTranscripts(true)
	if e := runnerEntryFromHello(tcid("R"), hello, 1, time.Now()); !e.Transcripts {
		t.Fatal("RunnerEntry.Transcripts not set from the hello")
	}
}

// Every field the server relays reaches the runner.
func TestRunnerTranscriptRequestCopiesEveryField(t *testing.T) {
	tid := protocol.TaskID{Id: [16]byte{1, 2, 3}}
	r := runnerTranscriptRequest(tid, protocol.TranscriptOp_Get, []byte("sess"), []byte("file"), "/repo", true, 77)
	if r.TaskId != tid || r.Op != protocol.TranscriptOp_Get || string(r.Session) != "sess" || string(r.File) != "file" ||
		string(r.RepoPath) != "/repo" || !r.IncludeShared() || r.StreamId != 77 {
		t.Fatalf("relayed %+v", r)
	}
}

// Names past the bounds are refused here with the status the runner would
// give, before a relay that could not fit one datagram is built — never an
// internal_error that depends on the transport (Task 5 review, I2).
func TestTranscriptOverlongNamesRefusedBeforeTheRunner(t *testing.T) {
	f := newTranscriptServerFixture(t, true)
	conn := f.operator(t)
	tid := hexToTaskID(t, f.taskHex)

	var list protocol.TranscriptListRequest
	list.TaskId = tid
	list.SetSession([]byte(strings.Repeat("s", protocol.TranscriptSessionMax+1)))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TranscriptList, RequestId: 4}}
	req.SetTranscriptList(list)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	if st := transcriptStatus(t, lastTaskControlResponse(t, conn)); st != protocol.TranscriptStatus_NoSuchSession {
		t.Fatalf("overlong session: %v, want no_such_session", st)
	}

	var get protocol.TranscriptGetRequest
	get.TaskId = tid
	get.SetSession([]byte("s"))
	get.SetFile([]byte(strings.Repeat("f", protocol.TranscriptFileMax+1)))
	req = &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TranscriptGet, RequestId: 5}}
	req.SetTranscriptGet(get)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	r := lastTaskControlResponse(t, conn)
	if g := r.TranscriptGet(); g == nil || g.Status != protocol.TranscriptStatus_NoSuchFile {
		t.Fatalf("overlong file: %+v, want no_such_file", r)
	}
	if n := len(f.sentTranscriptRequests(t)); n != 0 {
		t.Fatalf("%d requests reached the runner", n)
	}
}
