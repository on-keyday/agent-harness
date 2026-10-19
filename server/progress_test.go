package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// progressFixture is a handler with one agent task T whose connection's
// principal is T and whose caps are none — progress_set needs no capability.
type progressFixture struct {
	h    *TaskHandler
	tHex string
	conn *fakeConn
}

func newProgressFixture(t *testing.T) *progressFixture {
	t.Helper()
	h := newTestHandler(t)
	tHex := h.Tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9711-1"), nextSendStreamID: 10}
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): hexToTaskID(t, tHex)}
	return &progressFixture{h: h, tHex: tHex, conn: conn}
}

func (f *progressFixture) set(t *testing.T, text string, clear bool) *protocol.ProgressSetResponse {
	t.Helper()
	return progressSetAs(t, f.h, f.conn, text, clear)
}

func progressSetAs(t *testing.T, h *TaskHandler, conn *fakeConn, text string, clear bool) *protocol.ProgressSetResponse {
	t.Helper()
	before := len(conn.Sent())
	var ps protocol.ProgressSetRequest
	ps.SetClear(clear)
	ps.SetText([]byte(text))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ProgressSet, RequestId: 5}
	req.SetProgressSet(ps)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	waitFor(t, func() bool { return len(conn.Sent()) == before+1 })
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_ProgressSet || resp.ProgressSet() == nil {
		t.Fatalf("response kind = %v, want progress_set", resp.Kind)
	}
	return resp.ProgressSet()
}

func listRowAs(t *testing.T, h *TaskHandler, conn *fakeConn, taskHex string) (protocol.TaskInfo, bool) {
	t.Helper()
	before := len(conn.sendStreams)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{})
	h.Handle(conn, encodeTaskControlRequest(t, req))
	if len(conn.sendStreams) != before+1 {
		t.Fatalf("list opened %d send streams, want 1", len(conn.sendStreams)-before)
	}
	body := decodeListBody(t, conn.sendStreams[before].bytes)
	for _, ti := range body.Tasks {
		if hexOfTaskID(ti.Id) == taskHex {
			return ti, true
		}
	}
	return protocol.TaskInfo{}, false
}

func hexOfTaskID(id protocol.TaskID) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for _, x := range id.Id {
		b.WriteByte(digits[x>>4])
		b.WriteByte(digits[x&0xf])
	}
	return b.String()
}

func TestProgressSet_WritesCallersOwnTask(t *testing.T) {
	f := newProgressFixture(t)
	resp := f.set(t, "task A / stage 2", false)
	if resp.Status != protocol.ProgressSetStatus_Ok || resp.ProgressAt == 0 {
		t.Fatalf("response = %+v, want ok with a stamp", resp)
	}
	e, _ := f.h.Tasks.Get(f.tHex)
	if e.Progress != "task A / stage 2" || e.ProgressAt != int64(resp.ProgressAt) {
		t.Fatalf("store = %q@%d, want the written line at the returned stamp", e.Progress, e.ProgressAt)
	}
	row, ok := listRowAs(t, f.h, f.conn, f.tHex)
	if !ok {
		t.Fatal("caller's own row missing from ls")
	}
	if string(row.Progress) != "task A / stage 2" || row.ProgressAt != resp.ProgressAt || row.ProgressAgeMs >= 5000 {
		t.Fatalf("ls row progress=%q at=%d age=%dms", row.Progress, row.ProgressAt, row.ProgressAgeMs)
	}
}

func TestProgressSet_OperatorRefused(t *testing.T) {
	f := newProgressFixture(t)
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9712-1")}
	if resp := progressSetAs(t, f.h, op, "x", false); resp.Status != protocol.ProgressSetStatus_NoPrincipal {
		t.Fatalf("operator write status = %v, want no_principal", resp.Status)
	}
}

func TestProgressSet_BadText(t *testing.T) {
	cases := map[string]struct {
		text  string
		clear bool
	}{
		"257 bytes":       {strings.Repeat("a", 257), false},
		"ESC sequence":    {"\x1b[2J", false},
		"DEL":             {"a\x7f", false},
		"C1 NEL":          {"a\u0085", false},
		"invalid UTF-8":   {"\xff", false},
		"newline":         {"a\nb", false},
		"clear with text": {"x", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newProgressFixture(t)
			if resp := f.set(t, c.text, c.clear); resp.Status != protocol.ProgressSetStatus_BadText || resp.ProgressAt != 0 {
				t.Fatalf("status = %v at=%d, want bad_text with no stamp", resp.Status, resp.ProgressAt)
			}
			if e, _ := f.h.Tasks.Get(f.tHex); e.Progress != "" || e.ProgressAt != 0 {
				t.Fatalf("store changed to %q@%d on a refused write", e.Progress, e.ProgressAt)
			}
		})
	}
	// The boundary itself is accepted: exactly 256 bytes of multi-byte runes.
	f := newProgressFixture(t)
	if resp := f.set(t, strings.Repeat("あ", 85)+"a", false); resp.Status != protocol.ProgressSetStatus_Ok {
		t.Fatalf("256-byte line status = %v, want ok", resp.Status)
	}
}

func TestProgressSet_ClearKeepsStamp(t *testing.T) {
	f := newProgressFixture(t)
	first := f.set(t, "stage 1", false)
	time.Sleep(time.Millisecond)
	second := f.set(t, "", true)
	if second.Status != protocol.ProgressSetStatus_Ok || second.ProgressAt <= first.ProgressAt {
		t.Fatalf("clear = %+v after stamp %d, want ok with a newer stamp", second, first.ProgressAt)
	}
	if e, _ := f.h.Tasks.Get(f.tHex); e.Progress != "" || e.ProgressAt != int64(second.ProgressAt) {
		t.Fatalf("store after clear = %q@%d", e.Progress, e.ProgressAt)
	}
}

func TestProgressSet_SurvivesWALReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.wal")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewTaskStore()
	s.SetWAL(w)
	id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	if _, ok := s.SetProgress(id, "stage 3", 12345); !ok {
		t.Fatal("SetProgress on a live task returned false")
	}
	w.Close()

	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewTaskStore()
	r.ReplayEvents(events)
	e, ok := r.Get(id)
	if !ok || e.Progress != "stage 3" || e.ProgressAt != 12345 {
		t.Fatalf("replayed entry = %q@%d (found=%v), want stage 3@12345", e.Progress, e.ProgressAt, ok)
	}
}

func TestProgressSet_RedactedOnParentHop(t *testing.T) {
	h := newTestHandler(t)
	pHex := h.Tasks.Create("/repo", "parent", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	cHex := h.Tasks.Create("/repo", "child", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, pHex), "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	h.Tasks.SetProgress(pHex, "parent's private stage", 1)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9713-1"), nextSendStreamID: 10}
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): hexToTaskID(t, cHex)}

	row, ok := listRowAs(t, h, conn, pHex)
	if !ok {
		t.Fatal("child must see its parent's (redacted) row")
	}
	if len(row.Progress) != 0 || row.ProgressAt != 0 || len(row.Title) != 0 {
		t.Fatalf("parent-hop row progress=%q at=%d title=%q, want them emptied", row.Progress, row.ProgressAt, row.Title)
	}
}

// The event half of the parent-hop redaction (Task 2 left it here, where the
// store has a progress line to empty).
func TestProgressSet_EventRedactedForParentHop(t *testing.T) {
	s := makeEventServer(t)
	aHex := s.tasks.Create("/r", "a", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, Scope{}, "")
	kHex := s.tasks.Create("/r", "k", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, hexToTaskID(t, aHex), "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	const opCID, kCID = "ws:127.0.0.1:9921-1", "ws:127.0.0.1:9922-1"
	recordAgent(s, kCID, hexToTaskID(t, kHex))
	op := subscribeTasksStatus(t, s, opCID)
	k := subscribeTasksStatus(t, s, kCID)

	s.tasks.SetProgress(aHex, "secret stage", 7)
	s.tasks.OnHold(aHex)
	opEvs, kEvs := op.take(), k.take()
	if len(opEvs) < 1 || string(opEvs[len(opEvs)-1].Progress) != "secret stage" || opEvs[len(opEvs)-1].ProgressAt != 7 {
		t.Fatalf("operator events = %+v, want the last carrying the progress", opEvs)
	}
	if len(kEvs) < 1 {
		t.Fatal("K got no event for its parent")
	}
	for _, ev := range kEvs {
		if len(ev.Progress) != 0 || ev.ProgressAt != 0 {
			t.Fatalf("K's parent-hop event carries progress=%q at=%d, want emptied", ev.Progress, ev.ProgressAt)
		}
	}
}

func TestProgressSet_PublishesTaskProgress(t *testing.T) {
	f := newProgressFixture(t)
	var got []string
	f.h.OnTaskProgress = func(taskHex string) { got = append(got, taskHex) }
	f.set(t, "s", false)
	f.set(t, "\x1b", false) // refused: no event
	if len(got) != 1 || got[0] != f.tHex {
		t.Fatalf("OnTaskProgress calls = %v, want exactly [%s]", got, f.tHex)
	}
}

// A stamp is what await-progress's baseline compares against (D17), so it may
// never move backwards, even when two writes from one task race and the later
// clock reading reaches the store first.
func TestSetProgress_StampNeverGoesBackwards(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	if snap, _ := s.SetProgress(id, "b", 100); snap.ProgressAt != 100 {
		t.Fatalf("first stamp = %d, want 100", snap.ProgressAt)
	}
	snap, _ := s.SetProgress(id, "a", 50) // an earlier reading, applied later
	if snap.ProgressAt != 101 || snap.Progress != "a" {
		t.Fatalf("late write = %q@%d, want a@101", snap.Progress, snap.ProgressAt)
	}
}

// A stored stamp from a server clock that has since stepped backwards (a
// board without an RTC booting before NTP) must not read as an enormous age.
func TestProgressAgeMs_FutureStampIsZero(t *testing.T) {
	if got := progressAgeMs(time.Now().Add(time.Hour).UnixNano()); got != 0 {
		t.Fatalf("age of a future stamp = %d, want 0", got)
	}
}
