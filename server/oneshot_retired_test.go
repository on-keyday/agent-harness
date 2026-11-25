package server

import (
	"path/filepath"
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

// The retired kinds hold their ordinals and have no arm (F34). What an older
// client sends for one — a header plus the old body — does not decode, gets
// no answer, and creates nothing.
func TestRetiredKindsGetNoAnswer(t *testing.T) {
	for _, kind := range []protocol.TaskControlKind{
		protocol.TaskControlKind_RetiredSubmit,
		protocol.TaskControlKind_RetiredGetTaskLog,
	} {
		h := newTestHandler(t)
		conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9180-1"), nextStreamID: 100}
		hdr := protocol.TaskControlRequestHeader{Kind: kind, RequestId: 9}
		payload := append(hdr.MustAppend(nil), make([]byte, 16)...) // an old body: a task id or a repo path
		var req protocol.TaskControlRequest
		if err := req.DecodeExact(payload); err == nil {
			t.Errorf("%v: an old request decoded; the kind has an arm again", kind)
		}
		h.Handle(conn, payload)
		if n := len(conn.Sent()); n != 0 {
			t.Errorf("%v: %d messages sent, want none", kind, n)
		}
		if n := len(h.Tasks.List(0)); n != 0 {
			t.Errorf("%v: %d tasks created", kind, n)
		}
	}
}
