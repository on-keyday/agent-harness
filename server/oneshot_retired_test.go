package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A3 (A-D32): oneshot tasks stop being created and placed. Review Focus 2:
// a WAL holding them replays; the ones nothing would ever place or read fail.
func TestReplayRetiresQueuedAndHeldOneshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewTaskStore()
	s.SetWAL(w)
	mk := func() string {
		id, err := s.CreateWithinLimits("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "", RunSpec{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	queued, held, running, done := mk(), mk(), mk(), mk()
	for _, id := range []string{held, running, done} {
		s.Assign(id, testRunnerID("R"), "", false)
	}
	if err := s.MarkHold(held, "aa", "bb", 0); err != nil {
		t.Fatal(err)
	}
	s.Finish(done, 0, nil)
	_ = w.Close()

	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewTaskStore()
	r.ReplayEvents(events)
	for _, c := range []struct {
		id, name, msg string
		want          protocol.TaskStatus
	}{
		{queued, "queued", "oneshot_retired", protocol.TaskStatus_Failed},
		{held, "held", "oneshot_retired", protocol.TaskStatus_Failed},
		{running, "running", "server_restart", protocol.TaskStatus_Failed},
		{done, "done", "", protocol.TaskStatus_Succeeded},
	} {
		e, _ := r.Get(c.id)
		if e.Status != c.want || string(e.ErrorMsg) != c.msg || e.Kind != protocol.TaskKind_Oneshot {
			t.Errorf("%s: %v %q kind %v, want %v %q oneshot", c.name, e.Status, e.ErrorMsg, e.Kind, c.want, c.msg)
		}
	}
}

// An old client's submit is answered, not dropped: with the verb to use.
func TestSubmitIsRetired(t *testing.T) {
	h := newTestHandler(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9180-1"), nextStreamID: 100}
	var sub protocol.SubmitRequest
	sub.SetRepoPath([]byte("/r"))
	sub.SetPrompt([]byte("p"))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_Submit, RequestId: 9}}
	req.SetSubmit(sub)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	r := resp.Submit()
	if r == nil || r.Status != protocol.SubmitStatus_InternalError || !strings.Contains(string(r.ErrorMsg), "session new --oneshot") ||
		!strings.Contains(string(r.ErrorMsg), "--background") {
		t.Fatalf("submit answered %+v, want internal_error naming session new --oneshot", r)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("%d tasks created", n)
	}
}
