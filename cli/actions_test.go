package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func testAction(t *testing.T, id byte, name string, specs []string, words ...string) protocol.ActionRecord {
	t.Helper()
	var d protocol.ActionDefinition
	d.SetName([]byte(name))
	var ps []protocol.ActionParam
	for _, s := range specs {
		p, err := actiondef.ParseParam(s)
		if err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	d.SetParams(ps)
	els, err := actiondef.ParseTemplate(words, ps)
	if err != nil {
		t.Fatal(err)
	}
	d.SetTemplate(els)
	rec := protocol.ActionRecord{Revision: 3, Definition: d, CreatedAtMs: 1764234000000, EditedAtMs: 1764237600000}
	rec.Id.Id[0] = id
	rec.SetCreatedBy([]byte("operator"))
	rec.SetEditedBy([]byte("task:00112233445566778899aabbccddeeff"))
	return rec
}

func TestResolveActionRef(t *testing.T) {
	a := testAction(t, 0xa1, "deploy", nil, "true")
	b := testAction(t, 0xb2, "build", nil, "true")
	list := []protocol.ActionRecord{a, b}
	if r, err := ResolveActionRef(list, "build"); err != nil || r.Id != b.Id {
		t.Fatalf("by name: %v %v", r.Id, err)
	}
	if r, err := ResolveActionRef(list, ActionIDHex(a.Id)); err != nil || r.Id != a.Id {
		t.Fatalf("by id: %v %v", r.Id, err)
	}
	if _, err := ResolveActionRef(list, "nope"); err == nil || !strings.Contains(err.Error(), "action ls") {
		t.Fatalf("unknown: %v", err)
	}
	// A name that is another action's id is ambiguous, never guessed.
	c := testAction(t, 0xc3, ActionIDHex(a.Id), nil, "true")
	if _, err := ResolveActionRef([]protocol.ActionRecord{a, c}, ActionIDHex(a.Id)); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("id/name collision: %v", err)
	}
}

// Zero is a measurement: an action with no params prints params=0, and an
// empty store prints actions=0.
func TestFormatActionsNeverElidesZero(t *testing.T) {
	got := FormatActions([]protocol.ActionRecord{
		testAction(t, 0xa1, "deploy", []string{"env=choice:staging,prod", "ref=token"}, "./deploy.sh", "--env", "{env}", "--ref", "{ref}"),
		testAction(t, 0xb2, "uptime", nil, "uptime"),
	})
	for _, want := range []string{
		"a1000000000000000000000000000000 name=deploy rev=3 params=2",
		"  run: ./deploy.sh --env {env} --ref {ref}",
		"  param env: choice staging|prod",
		"  param ref: token",
		"b2000000000000000000000000000000 name=uptime rev=3 params=0",
		"actions=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if got := FormatActions(nil); got != "actions=0\n" {
		t.Errorf("empty: %q", got)
	}
}

func TestFormatActionsJSON(t *testing.T) {
	got := FormatActionsJSON([]protocol.ActionRecord{testAction(t, 0xb2, "uptime", nil, "uptime")})
	for _, want := range []string{`"id":"b2000000000000000000000000000000"`, `"name":"uptime"`, `"revision":3`, `"params":[]`, `"template":["uptime"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestActionTemplateWordsRoundTrip(t *testing.T) {
	rec := testAction(t, 1, "x", []string{"env=token"}, "run", `\{env}`, "{env}", "a b")
	words := ActionTemplateWords(&rec.Definition)
	els, err := actiondef.ParseTemplate(words, rec.Definition.Params)
	if err != nil {
		t.Fatalf("re-parse %q: %v", words, err)
	}
	if !reflect.DeepEqual(els, rec.Definition.Template) {
		t.Fatalf("round trip:\n%+v\nwant\n%+v", els, rec.Definition.Template)
	}
	if s := ActionTemplateString(&rec.Definition); s != `run \{env} {env} "a b"` {
		t.Fatalf("display %q", s)
	}
}

func TestExecRunStatusErrorNewStatuses(t *testing.T) {
	if err := execRunStatusError("t", protocol.ExecRunStatus_NoSuchAction); err == nil || !strings.Contains(err.Error(), "no such action") {
		t.Errorf("no_such_action: %v", err)
	}
	if err := execRunStatusError("t", protocol.ExecRunStatus_OutsideCondition); err == nil || !strings.Contains(err.Error(), "condition") {
		t.Errorf("outside_condition: %v", err)
	}
	if err := execRunStatusError("t", protocol.ExecRunStatus_BadActionValues); err == nil || !strings.Contains(err.Error(), "edited") ||
		strings.Contains(err.Error(), "could not read") {
		t.Errorf("bad_action_values: %v", err)
	}
}

func TestExecRunInfoLinesShowAction(t *testing.T) {
	var info protocol.ExecRunInfo
	info.ExecId = 4
	info.SetActionPresent(true)
	info.SetActionId(protocol.ActionID{Id: [16]byte{0xa1}})
	info.SetActionRevision(2)
	info.SetActionName([]byte("deploy"))
	lines := strings.Join(ExecRunInfoLines([]protocol.ExecRunInfo{info}), "\n")
	if !strings.Contains(lines, "action=deploy@r2") {
		t.Fatalf("listing lacks the action:\n%s", lines)
	}
	js := ExecRunInfoJSONLine(&info)
	for _, want := range []string{`"action_id":"a1000000000000000000000000000000"`, `"action_revision":2`, `"action_name":"deploy"`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON lacks %s: %s", want, js)
		}
	}
	var raw protocol.ExecRunInfo
	if js := ExecRunInfoJSONLine(&raw); !strings.Contains(js, `"action_id":""`) {
		t.Errorf("a raw-argv row must say it has no action: %s", js)
	}
}

// ActionInvocationFor checks values client-side, with the server's rule.
func TestActionInvocationFromRecord(t *testing.T) {
	rec := testAction(t, 1, "hello", []string{"who=token"}, "echo", "{who}")
	inv, err := ActionInvocationFromRecord(rec, map[string]string{"who": "world"})
	if err != nil || inv.Id != rec.Id || len(inv.Values) != 1 || string(inv.Values[0].Bytes) != "world" {
		t.Fatalf("invocation: %+v %v", inv, err)
	}
	if _, err := ActionInvocationFromRecord(rec, map[string]string{"who": "-x"}); err == nil {
		t.Fatal("a token starting with - was accepted client-side")
	}
	if _, err := ActionInvocationFromRecord(rec, nil); err == nil || !strings.Contains(err.Error(), "who") {
		t.Fatalf("missing value: %v", err)
	}
}

// The body an action exec sends: no argv, the invocation, nothing else.
func TestExecRunSendsActionVariant(t *testing.T) {
	inv := protocol.ActionInvocation{Id: protocol.ActionID{Id: [16]byte{7}}}
	var v protocol.ActionValue
	v.SetBytes([]byte("prod"))
	inv.SetValues([]protocol.ActionValue{v})
	body := execRunBody(nil, ExecRunOpts{Action: &inv})
	if body.Argv.ArgvLen != 0 || !body.ActionPresent() || body.Action() == nil || body.Action().Id != inv.Id ||
		string(body.Action().Values[0].Bytes) != "prod" {
		t.Fatalf("body = %+v", body)
	}
	raw := execRunBody([]string{"echo", "x"}, ExecRunOpts{})
	if raw.ActionPresent() || raw.Argv.ArgvLen != 2 {
		t.Fatalf("raw body = %+v", raw)
	}
}

func TestExecRunRefusesActionWithArgvOrShell(t *testing.T) {
	inv := &protocol.ActionInvocation{}
	for name, tc := range map[string]struct {
		argv []string
		opts ExecRunOpts
	}{
		"argv beside an action":  {[]string{"echo"}, ExecRunOpts{Action: inv}},
		"shell beside an action": {nil, ExecRunOpts{Action: inv, ShellLine: true}},
		"pty beside an action":   {nil, ExecRunOpts{Action: inv, Pty: true}},
	} {
		if _, err := (&Client{}).ExecRun(context.Background(), "0123456789abcdef0123456789abcdef", tc.argv, tc.opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The overlap warning (grant conditions spec, § Surfaces): printed on the row
// only when the server reports one; always present in JSON, [] included.
func TestListRowShowsActionOverlap(t *testing.T) {
	var info protocol.TaskInfo
	info.ActionOverlap = protocol.Capability_FileWrite | protocol.Capability_ExecCowrite
	if got := taskLine(info, nil); !strings.Contains(got, "  warn=action_overlap:") || !strings.Contains(got, "file_write") {
		t.Fatalf("row lacks the warning:\n%s", got)
	}
	if got := taskLine(protocol.TaskInfo{}, nil); strings.Contains(got, "action_overlap") {
		t.Fatalf("a grant with no overlap warns:\n%s", got)
	}
	j := newTaskJSON(&info, nil)
	if !reflect.DeepEqual(j.ActionOverlap, []string{"file_write", "exec_cowrite"}) {
		t.Fatalf("json action_overlap = %v", j.ActionOverlap)
	}
	if j := newTaskJSON(&protocol.TaskInfo{}, nil); j.ActionOverlap == nil || len(j.ActionOverlap) != 0 {
		t.Fatalf("json action_overlap for none = %#v, want []", j.ActionOverlap)
	}
}
