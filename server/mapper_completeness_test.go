package server

import (
	"reflect"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// toTaskInfo / toRunnerInfo copy a store entry into its wire struct field by
// field, by hand. Nothing makes the compiler notice when a new field is added to
// the entry + the wire struct but its assignment is forgotten — the field just
// stays zero and every operator surface silently shows a default. That is not
// hypothetical: BOTH mappers shipped exactly that bug on 2026-07-16
// (toRunnerInfo dropped AgentProfiles, so TUI/WebUI agent pickers could not
// offer codex; toTaskInfo dropped AgentProfile, so a task resumed under codex
// still displayed as claude). Reviews missed both — they checked that the field
// existed and that clients read it, never that the server wrote it.
//
// These tests fill EVERY field of the entry and then assert no field of the
// mapped wire struct is left zero. Add a field and forget the mapper → red.
// Anything legitimately zero must be named in the skip map WITH a reason, so the
// exemption is a deliberate, reviewable act rather than an oversight.

func assertNoZeroFields(t *testing.T, v any, skip map[string]string) {
	t.Helper()
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		name := rt.Field(i).Name
		if reason, ok := skip[name]; ok {
			if reason == "" {
				t.Errorf("%s.%s is skipped without a reason — every exemption must be justified", rt.Name(), name)
			}
			continue
		}
		if rv.Field(i).IsZero() {
			t.Errorf("%s.%s is ZERO after mapping a fully-populated entry — the mapper forgot it "+
				"(add the assignment, or add it to the skip map with a reason)", rt.Name(), name)
		}
	}
}

func TestToTaskInfoMapsEveryField(t *testing.T) {
	now := time.Now()
	ec := int32(3)
	e := TaskEntry{
		ID:            "00112233445566778899aabbccddeeff",
		RepoPath:      "/repo",
		Prompt:        "do it",
		Kind:          protocol.TaskKind_Interactive,
		OriginKind:    protocol.ClientKind_Tui,
		ResumedByKind: protocol.ClientKind_Cli,
		CreatorTaskID: protocol.TaskID{Id: [16]byte{1}},
		Capabilities:  protocol.Capability_All,
		// Non-default on purpose: the default scope is the ZERO TaskScope, so a
		// mapper that forgot Scope would still pass against defaultScope().
		Scope:           Scope{Base: protocol.ScopeBase_None, IDs: []string{"00112233445566778899aabbccddeeff"}},
		AgentProfile:    "codex",
		Status:          protocol.TaskStatus_Running,
		AssignedTo:      "ws:127.0.0.1:8539-1",
		WorktreeDir:     "/wt",
		CreatedAt:       now,
		StartedAt:       &now,
		EndedAt:         &now,
		ExitCode:        &ec,
		ErrorMsg:        []byte("boom"),
		IsAttached:      true,
		RingBufferBytes: 4096,
	}
	info := toTaskInfo(e)
	assertNoZeroFields(t, info, map[string]string{
		// Live-session telemetry: not part of the entry→wire copy. The list
		// handler fills these from the session registry after toTaskInfo.
		"LastOutputAt": "filled by the list handler from live session state, not from TaskEntry",
		"OutputIdleMs": "filled by the list handler from live session state, not from TaskEntry",
	})
}

func TestToRunnerInfoMapsEveryField(t *testing.T) {
	now := time.Now()
	e := RunnerEntry{
		ID:             "ws:127.0.0.1:8539-1",
		Hostname:       "gmkhost",
		AllowedRoots:   []string{"/repo"},
		MaxTasks:       8,
		AgentBin:       "claude",
		AgentProfiles:  []string{"claude", "codex"},
		SkillsInjected: true,
		ActiveTasks:    map[string]struct{}{"00112233445566778899aabbccddeeff": {}},
		ConnectedAt:    now,
		LastSeen:       now,
		Conn:           &fakeConn{id: buildTestCID("ws:127.0.0.1:8539-1")},
	}
	info := toRunnerInfo(e)
	assertNoZeroFields(t, info, map[string]string{
		// RunnerStatus_Idle is the zero value, and an entry with capacity is
		// legitimately Idle — a non-zero assertion cannot say anything here.
		"Status": "RunnerStatus_Idle is the zero value; Status() is covered by its own tests",
	})
}

// TestPortForwardInfoMapsEveryField is the same completeness guard for
// portForwardInfo (server/port_forward_list.go), the third hand-written
// entry→wire mapper in this package alongside toTaskInfo/toRunnerInfo above.
func TestPortForwardInfoMapsEveryField(t *testing.T) {
	pf := &portForward{
		forwardID:  42,
		direction:  protocol.PortForwardDirection_Remote,
		taskIDHex:  "00112233445566778899aabbccddeeff",
		runnerID:   "ws:127.0.0.1:8539-1",
		clientCID:  "ws:127.0.0.1:9-1",
		clientKind: protocol.ClientKind_Cli,
		bindAddr:   "127.0.0.1",
		bindPort:   8080,
		targetHost: "db.internal",
		targetPort: 5432,
		// InProcess (1), not the zero value OsSocket (0), so a forgotten
		// mapping shows up as ZERO rather than being indistinguishable from a
		// correctly-mapped OsSocket.
		clientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	info := portForwardInfo(pf)
	assertNoZeroFields(t, info, map[string]string{})
}

// WhoAmIResponse is built by hand in the Whoami dispatch case, with the same
// exposure as toTaskInfo: a field added to the wire struct and forgotten at the
// build site round-trips as its zero and every client shows a default. That is
// not hypothetical either — Scope was added to the schema and left unpopulated
// here, and the symptom was `whoami` printing no scope for a task that had one.
//
// This asserts the handler fills every field, using a confined principal so
// nothing is legitimately zero.
func TestWhoamiResponseMapsEveryField(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	// A grandchild: non-zero principal AND non-zero creator.
	child := h.Tasks.Create("/r", "gc", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, c), "", protocol.RunnerSelector{}, nil,
		protocol.Capability_Spawn, Scope{Base: protocol.ScopeBase_None}, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9960-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, child)

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Whoami, RequestId: 1}
	req.SetWhoami(protocol.WhoAmIRequest{})
	h.Handle(conn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, conn)
	w := resp.Whoami()
	if w == nil {
		t.Fatalf("no whoami response (kind %v)", resp.Kind)
	}
	assertNoZeroFields(t, *w, map[string]string{})
}
