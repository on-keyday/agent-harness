package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Grant conditions are stored with the override they belong to, in the WAL and
// in the schedule store (grant conditions spec C6), and an unreadable stored
// body reaches nothing (C7).

func portsCond(t *testing.T, spans ...authority.PortSpan) *authority.Condition {
	t.Helper()
	c, err := authority.NewCondition(protocol.ConditionKind_PortRange, nil, spans)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func actionsCond(t *testing.T, ids ...byte) *authority.Condition {
	t.Helper()
	var a [][16]byte
	for _, b := range ids {
		var id [16]byte
		id[0] = b
		a = append(a, id)
	}
	c, err := authority.NewCondition(protocol.ConditionKind_ActionSet, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func condScope(fwd, run *authority.Condition) authority.Scope {
	return authority.Scope{Base: protocol.ScopeBase_Subtree, Overrides: []authority.ScopeOverride{
		{Caps: protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote, Base: protocol.ScopeBase_Global, Cond: fwd},
		{Caps: protocol.Capability_ExecRun, Base: protocol.ScopeBase_Global, Cond: run},
	}}
}

func sameCond(a, b *authority.Condition) bool { return a.Equal(b) }

func TestReplayKeepsConditions(t *testing.T) {
	s, path := provenanceStore(t)
	scope := condScope(portsCond(t, authority.PortSpan{Lo: 8000, Hi: 8100}), actionsCond(t, 1, 2))
	id := s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: scope}, "")
	narrowed := condScope(portsCond(t, authority.PortSpan{Lo: 8050, Hi: 8060}), actionsCond(t, 1))
	if _, ok := s.SetCaps(id, GrantUpdate{Scope: true, To: authority.Grant{Scope: narrowed}}, AuthorityWrite{}); !ok {
		t.Fatal("SetCaps refused")
	}

	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents(events)
	got, ok := fresh.Get(id)
	if !ok {
		t.Fatal("task lost on replay")
	}
	for _, bit := range []protocol.Capability{protocol.Capability_ForwardLocal, protocol.Capability_ExecRun} {
		if !sameCond(got.Scope.CondFor(bit), narrowed.CondFor(bit)) {
			t.Errorf("%s: replayed %+v, want %+v", bit, got.Scope.CondFor(bit), narrowed.CondFor(bit))
		}
	}
}

// A stored condition this build cannot read — an unknown kind, a marker with
// an empty body, a span written backwards — replays as reaching nothing,
// never as unconditioned.
func TestWALUnknownConditionKindReplaysAsReachingNothing(t *testing.T) {
	for name, cond := range map[string]string{
		"unknown kind":   `{"kind":9}`,
		"no body":        `{}`,
		"backwards span": `{"kind":2,"spans":[[9,8]]}`,
		"bad action hex": `{"kind":1,"actions":["zz"]}`,
	} {
		var ev WALEvent
		if err := json.Unmarshal([]byte(`{"type":"task_created","task_id":"`+boundX+
			`","repo_path":"/r","ts":1,"capabilities":32,"scope_overrides":[{"caps":32,"base":2,"cond":`+cond+`}]}`), &ev); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fresh := NewTaskStore()
		fresh.ReplayEvents([]WALEvent{ev})
		got, ok := fresh.Get(boundX)
		if !ok {
			t.Fatalf("%s: record lost", name)
		}
		c := got.Scope.CondFor(protocol.Capability_ForwardLocal)
		if c == nil || c.Kind != 0 {
			t.Errorf("%s: replayed as %+v, want kind 0 (reaches nothing)", name, c)
		}
	}
	// A record from before conditions has no cond key: unconditioned.
	var ev WALEvent
	if err := json.Unmarshal([]byte(`{"type":"task_created","task_id":"`+boundX+
		`","repo_path":"/r","ts":1,"capabilities":32,"scope_overrides":[{"caps":32,"base":2}]}`), &ev); err != nil {
		t.Fatal(err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents([]WALEvent{ev})
	if got, _ := fresh.Get(boundX); got.Scope.CondFor(protocol.Capability_ForwardLocal) != nil {
		t.Error("a record without a cond key replayed as conditioned")
	}
}

func TestScheduleStoreKeepsConditions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Unbound, "")
	sc.Grant.Scope = condScope(portsCond(t, authority.PortSpan{Lo: 22, Hi: 22}), actionsCond(t, 7))
	if err := st.Put(sc); err != nil {
		t.Fatal(err)
	}
	re, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := re.Get(sc.ID)
	if !ok {
		t.Fatal("schedule lost")
	}
	for _, bit := range []protocol.Capability{protocol.Capability_ForwardRemote, protocol.Capability_ExecRun} {
		if !sameCond(got.Grant.Scope.CondFor(bit), sc.Grant.Scope.CondFor(bit)) {
			t.Errorf("%s: loaded %+v, want %+v", bit, got.Grant.Scope.CondFor(bit), sc.Grant.Scope.CondFor(bit))
		}
	}
}

// The v1 file, written before conditions, has no cond key: it loads with every
// override unconditioned.
func TestScheduleFileWithoutCondLoadsUnconditioned(t *testing.T) {
	raw := schedulesFixtureV1
	for _, k := range []string{`, "cond": {"kind": 2, "spans": [[8000, 8100]]}`, `, "cond": {"kind": 1, "actions": ["0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"]}`} {
		if !strings.Contains(raw, k) {
			t.Fatalf("fixture lost %s; this test strips it to get a pre-condition file", k)
		}
		raw = strings.ReplaceAll(raw, k, "")
	}
	path := filepath.Join(t.TempDir(), "schedules.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(scheduleID(1))
	for _, o := range append(got.Grant.Scope.Overrides, got.Grant.DelegateScope.Overrides...) {
		if o.Cond != nil {
			t.Errorf("override %s loaded conditioned from a file without cond", o.Caps)
		}
	}
}

func TestCloneScopeCopiesCondition(t *testing.T) {
	src := condScope(portsCond(t, authority.PortSpan{Lo: 1, Hi: 2}), nil)
	cp := cloneScope(src)
	cp.Overrides[0].Cond.Spans[0].Hi = 999
	if src.Overrides[0].Cond.Spans[0].Hi != 2 {
		t.Fatal("cloneScope shares the condition with its source")
	}
}

// show streams the stored authority's bodies, and the inline overrides carry
// only the marker.
func TestScheduleDetailCarriesConditionBodies(t *testing.T) {
	e := newSchedEnv(t)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Unbound, "")
	sc.Grant.Scope = condScope(portsCond(t, authority.PortSpan{Lo: 22, Hi: 22}), nil)
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	conn := schedOpConn()
	r := e.call(t, conn, protocol.TaskControlKind_ScheduleShow, sc.ID)
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("show: %v", r.Status)
	}
	var d protocol.ScheduleDetail
	conn.streamBody(t, r.StreamId, &d)
	if len(d.Overrides) != 2 || !d.Overrides[0].Conditioned() || d.Overrides[1].Conditioned() {
		t.Fatalf("overrides %+v, want the first marked", d.Overrides)
	}
	if len(d.GrantConditions) != 1 || d.GrantConditions[0].Caps != sc.Grant.Scope.Overrides[0].Caps ||
		d.GrantConditions[0].PortRange() == nil || d.GrantConditions[0].PortRange().Spans[0].Lo != 22 {
		t.Fatalf("grant conditions %+v, want the port range on the forward override", d.GrantConditions)
	}
}
