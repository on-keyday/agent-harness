package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// formOf is the form a screen submits after opening a view unchanged.
func formOf(v ActionView) ActionForm {
	return ActionForm{Ref: v.ID, Revision: v.Revision, Name: v.Name, Description: v.Description, Params: v.Params, Template: v.Template}
}

// A record shown on a screen and submitted unchanged is the same definition,
// including a literal that looks like a reference (no escaping on a form).
func TestActionViewsRoundTripThroughTheForm(t *testing.T) {
	rec := testAction(t, 0xa1, "deploy", []string{"env=choice:staging,prod", "ref=token"}, "./deploy.sh", `\{env}`, "{env}", "--ref", "{ref}")
	rec.Definition.SetDescription([]byte("ship it"))
	views := ActionViews([]protocol.ActionRecord{rec})
	if len(views) != 1 || views[0].ID != ActionIDHex(rec.Id) || views[0].TemplateText != ActionTemplateString(&rec.Definition) {
		t.Fatalf("view: %+v", views)
	}
	in, err := ActionPutInputFromForm(formOf(views[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !in.Edit || in.Ref != views[0].ID || in.Revision != 3 || !in.DescriptionSet {
		t.Fatalf("input: %+v", in)
	}
	def, err := buildActionDefinition(in, in.Name, in.Description, "edit")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(def, rec.Definition) {
		t.Fatalf("round trip:\n%+v\nwant\n%+v", def, rec.Definition)
	}
}

// What the server refuses, the form refuses before sending, naming the rule.
func TestActionPutInputFromFormRefuses(t *testing.T) {
	lit := func(s string) ActionElementView { return ActionElementView{Literal: s} }
	par := func(n string) ActionElementView { return ActionElementView{IsParam: true, Param: n} }
	env := ActionParamView{Name: "env", Type: "choice", Choices: []string{"a", "b"}}
	for name, tc := range map[string]struct {
		f    ActionForm
		want string
	}{
		"param first":      {ActionForm{Name: "x", Params: []ActionParamView{env}, Template: []ActionElementView{par("env")}}, "literal"},
		"unused param":     {ActionForm{Name: "x", Params: []ActionParamView{env}, Template: []ActionElementView{lit("run")}}, "env"},
		"no choices":       {ActionForm{Name: "x", Params: []ActionParamView{{Name: "env", Type: "choice"}}, Template: []ActionElementView{lit("run"), par("env")}}, "choices"},
		"comma in choice":  {ActionForm{Name: "x", Params: []ActionParamView{{Name: "env", Type: "choice", Choices: []string{"a,b"}}}, Template: []ActionElementView{lit("run"), par("env")}}, "','"},
		"undeclared param": {ActionForm{Name: "x", Template: []ActionElementView{lit("run"), par("nope")}}, "nope"},
		"unknown type":     {ActionForm{Name: "x", Params: []ActionParamView{{Name: "env", Type: "free"}}, Template: []ActionElementView{lit("run"), par("env")}}, "free"},
	} {
		if _, err := ActionPutInputFromForm(tc.f); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, tc.want)
		}
	}
	ok := ActionForm{Name: "x", Params: []ActionParamView{env}, Template: []ActionElementView{lit("run"), par("env")}}
	in, err := ActionPutInputFromForm(ok)
	if err != nil || in.Edit {
		t.Fatalf("a valid new form: %+v %v", in, err)
	}
	if _, err := actiondef.ParseParam("env=choice:a,b"); err != nil {
		t.Fatal(err) // the CLI spelling of the same param still parses
	}
}

func TestUnenrollOfferNeedsForceOnlyOnInUse(t *testing.T) {
	users := []RunnerKeyUser{{ID: protocol.RunnerID{Id: [16]byte{0xaa}}, Hostname: "h1"}}
	res := RunnerUnenrollResult{Key: make([]byte, 32), Runners: users}
	if o := UnenrollOffer(protocol.RunnerKeyStatus_Ok, res); !o.OK || o.NeedsForce {
		t.Fatalf("ok: %+v", o)
	}
	o := UnenrollOffer(protocol.RunnerKeyStatus_InUse, res)
	if o.OK || !o.NeedsForce || len(o.InTheWay) != 1 || !strings.Contains(o.InTheWay[0], "aa000000") || !strings.Contains(o.InTheWay[0], "h1") {
		t.Fatalf("in_use: %+v", o)
	}
	if o := UnenrollOffer(protocol.RunnerKeyStatus_NotFound, RunnerUnenrollResult{}); o.OK || o.NeedsForce || o.Text == "" {
		t.Fatalf("not_found: %+v", o)
	}
}

func TestKickOfferNeedsForceOnlyOnInUse(t *testing.T) {
	res := RunnerKickResult{Runner: RunnerKeyUser{Hostname: "h1"}, Tasks: []protocol.TaskID{{Id: [16]byte{0xbb}}}}
	if o := KickOffer(protocol.RunnerKickStatus_Left, res); !o.OK || o.NeedsForce {
		t.Fatalf("left: %+v", o)
	}
	o := KickOffer(protocol.RunnerKickStatus_InUse, res)
	if o.OK || !o.NeedsForce || len(o.InTheWay) != 1 || !strings.HasPrefix(o.InTheWay[0], "bb00") {
		t.Fatalf("in_use: %+v", o)
	}
	if o := KickOffer(protocol.RunnerKickStatus_NotFound, RunnerKickResult{}); o.OK || o.NeedsForce {
		t.Fatalf("not_found: %+v", o)
	}
}

func TestRunnerViews(t *testing.T) {
	k := RunnerKeyViews([]RunnerKeyRow{{Key: make([]byte, 32), Label: "l", EnrolledBy: "operator", Bound: []protocol.TaskID{{Id: [16]byte{0xcc}}}}})
	if len(k) != 1 || k[0].Label != "l" || len(k[0].Bound) != 1 || !strings.HasPrefix(k[0].Bound[0], "cc") || k[0].Prefix == "" {
		t.Fatalf("key views: %+v", k)
	}
	d := RunnerDialViews([]RunnerDialRow{{ID: 7, Key: make([]byte, 32), Candidates: []string{"h:1"}, State: protocol.RunnerDialState_Waiting}})
	if len(d) != 1 || d[0].ID != FormatDialID(7) || d[0].State == "" || d[0].KeyPrefix == "" {
		t.Fatalf("dial views: %+v", d)
	}
}
