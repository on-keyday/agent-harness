package actiondef

import (
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func lit(s string) protocol.ActionLiteral {
	var l protocol.ActionLiteral
	l.SetBytes([]byte(s))
	return l
}

func litElem(s string) protocol.ActionElement {
	e := protocol.ActionElement{Kind: protocol.ActionElementKind_Literal}
	e.SetLiteral(lit(s))
	return e
}

func paramElem(i uint8) protocol.ActionElement {
	e := protocol.ActionElement{Kind: protocol.ActionElementKind_Param}
	e.SetParamIndex(i)
	return e
}

func choiceParam(name string, choices ...string) protocol.ActionParam {
	p := protocol.ActionParam{Type: protocol.ActionParamType_Choice}
	p.SetName([]byte(name))
	var ch protocol.ActionChoices
	var ls []protocol.ActionLiteral
	for _, c := range choices {
		ls = append(ls, lit(c))
	}
	ch.SetChoices(ls)
	p.SetChoices(ch)
	return p
}

func tokenParam(name string) protocol.ActionParam {
	p := protocol.ActionParam{Type: protocol.ActionParamType_Token}
	p.SetName([]byte(name))
	return p
}

// deploy is ./deploy.sh --env {env} --ref {ref}.
func deploy() protocol.ActionDefinition {
	var d protocol.ActionDefinition
	d.SetName([]byte("deploy"))
	d.SetParams([]protocol.ActionParam{choiceParam("env", "staging", "prod env"), tokenParam("ref")})
	d.SetTemplate([]protocol.ActionElement{litElem("./deploy.sh"), litElem("--env"), paramElem(0), litElem("--ref"), paramElem(1)})
	return d
}

func val(s string) protocol.ActionValue {
	var v protocol.ActionValue
	v.SetBytes([]byte(s))
	return v
}

func TestValidTokenC12(t *testing.T) {
	for _, bad := range []string{"", "-x", strings.Repeat("a", 129), "a b", "a/b", "é", "a;b", "$(x)"} {
		if ValidToken([]byte(bad)) {
			t.Errorf("ValidToken(%q) = true", bad)
		}
	}
	for _, good := range []string{"v1.2_rc-3", strings.Repeat("a", 128), "x", "0", ".hidden"} {
		if !ValidToken([]byte(good)) {
			t.Errorf("ValidToken(%q) = false", good)
		}
	}
}

func TestValidateAcceptsDeploy(t *testing.T) {
	d := deploy()
	if err := Validate(&d); err != nil {
		t.Fatalf("Validate(deploy) = %v", err)
	}
}

// Each limit at its edge: the error names the rule, so a client can say why.
func TestValidateLimits(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *protocol.ActionDefinition)
		want   string // a fragment of the error
	}{
		{"empty name", func(d *protocol.ActionDefinition) { d.SetName(nil) }, "name"},
		{"upper-case name", func(d *protocol.ActionDefinition) { d.SetName([]byte("Deploy")) }, "name"},
		{"name 65 bytes", func(d *protocol.ActionDefinition) { d.SetName([]byte(strings.Repeat("a", 65))) }, "name"},
		{"name starting with dot", func(d *protocol.ActionDefinition) { d.SetName([]byte(".x")) }, "name"},
		{"bad param name", func(d *protocol.ActionDefinition) { d.Params[1].SetName([]byte("Ref")) }, "param name"},
		{"param name 33 bytes", func(d *protocol.ActionDefinition) { d.Params[1].SetName([]byte("r" + strings.Repeat("a", 32))) }, "param name"},
		{"duplicate param name", func(d *protocol.ActionDefinition) { d.Params[1].SetName([]byte("env")) }, "param name"},
		{"17 params", func(d *protocol.ActionDefinition) {
			ps := []protocol.ActionParam{}
			el := []protocol.ActionElement{litElem("x")}
			for i := 0; i < 17; i++ {
				ps = append(ps, tokenParam("p"+string(rune('a'+i))))
				el = append(el, paramElem(uint8(i)))
			}
			d.SetParams(ps)
			d.SetTemplate(el)
		}, "params"},
		{"no template", func(d *protocol.ActionDefinition) { d.SetParams(nil); d.SetTemplate(nil) }, "template"},
		{"65 elements", func(d *protocol.ActionDefinition) {
			el := []protocol.ActionElement{}
			for i := 0; i < 65; i++ {
				el = append(el, litElem("x"))
			}
			d.SetParams(nil)
			d.SetTemplate(el)
		}, "template"},
		{"element 0 is a param", func(d *protocol.ActionDefinition) {
			d.SetTemplate([]protocol.ActionElement{paramElem(0), litElem("--ref"), paramElem(1)})
		}, "program"},
		{"param index out of range", func(d *protocol.ActionDefinition) {
			d.SetTemplate(append(d.Template, paramElem(2)))
		}, "param index"},
		{"33 choices", func(d *protocol.ActionDefinition) {
			var cs []string
			for i := 0; i < 33; i++ {
				cs = append(cs, "c"+strings.Repeat("x", i))
			}
			d.Params[0] = choiceParam("env", cs...)
		}, "choices"},
		{"no choices", func(d *protocol.ActionDefinition) { d.Params[0] = choiceParam("env") }, "choices"},
		{"256-byte choice", func(d *protocol.ActionDefinition) { d.Params[0] = choiceParam("env", strings.Repeat("a", 256)) }, "choice"},
		{"empty choice", func(d *protocol.ActionDefinition) { d.Params[0] = choiceParam("env", "") }, "choice"},
		{"duplicate choice", func(d *protocol.ActionDefinition) { d.Params[0] = choiceParam("env", "a", "a") }, "choice"},
		{"4097-byte literal", func(d *protocol.ActionDefinition) { d.Template[1] = litElem(strings.Repeat("a", 4097)) }, "literal"},
		{"empty literal", func(d *protocol.ActionDefinition) { d.Template[1] = litElem("") }, "literal"},
		{"NUL in a literal", func(d *protocol.ActionDefinition) { d.Template[1] = litElem("a\x00b") }, "NUL"},
		{"1025-byte description", func(d *protocol.ActionDefinition) { d.SetDescription([]byte(strings.Repeat("a", 1025))) }, "description"},
		{"unused param", func(d *protocol.ActionDefinition) {
			d.SetTemplate([]protocol.ActionElement{litElem("./deploy.sh"), paramElem(0)})
		}, "not used"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := deploy()
			tc.mutate(&d)
			err := Validate(&d)
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
	// The edges themselves are accepted.
	d := deploy()
	d.SetName([]byte("a" + strings.Repeat("b", 63)))
	d.SetDescription([]byte(strings.Repeat("a", 1024)))
	d.Template[1] = litElem(strings.Repeat("a", 4096))
	d.Params[0] = choiceParam("env", strings.Repeat("a", 255))
	if err := Validate(&d); err != nil {
		t.Fatalf("edges refused: %v", err)
	}
}

func TestResolveOneElementPerValue(t *testing.T) {
	d := deploy()
	argv, err := Resolve(&d, []protocol.ActionValue{val("prod env"), val("v1.2")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"./deploy.sh", "--env", "prod env", "--ref", "v1.2"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for _, tc := range []struct {
		name   string
		values []protocol.ActionValue
	}{
		{"too few values", []protocol.ActionValue{val("prod env")}},
		{"too many values", []protocol.ActionValue{val("prod env"), val("v1"), val("x")}},
		{"choice not in the list", []protocol.ActionValue{val("dev"), val("v1")}},
		{"bad token", []protocol.ActionValue{val("staging"), val("-rf")}},
		{"empty token", []protocol.ActionValue{val("staging"), val("")}},
	} {
		if _, err := Resolve(&d, tc.values); err == nil {
			t.Errorf("%s: Resolve accepted it", tc.name)
		}
	}
}

func TestParseTemplate(t *testing.T) {
	params := []protocol.ActionParam{choiceParam("env", "a"), tokenParam("ref")}
	els, err := ParseTemplate([]string{"./deploy.sh", "--env", "{env}", `\{env}`, `\{other}`, "{ref}"}, params)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	if len(els) != 6 {
		t.Fatalf("%d elements, want 6", len(els))
	}
	if p := els[2].ParamIndex(); els[2].Kind != protocol.ActionElementKind_Param || p == nil || *p != 0 {
		t.Errorf("{env} = %+v, want param 0", els[2])
	}
	if l := els[3].Literal(); l == nil || string(l.Bytes) != "{env}" {
		t.Errorf(`\{env} = %+v, want the literal {env}`, els[3])
	}
	if l := els[4].Literal(); l == nil || string(l.Bytes) != "{other}" {
		t.Errorf(`\{other} = %+v, want the literal {other}`, els[4])
	}
	if p := els[5].ParamIndex(); p == nil || *p != 1 {
		t.Errorf("{ref} = %+v, want param 1", els[5])
	}
	if _, err := ParseTemplate([]string{"./deploy.sh", "--env={env}"}, params); err == nil || !strings.Contains(err.Error(), "--env={env}") {
		t.Errorf("partial reference: err = %v, want a refusal naming the word", err)
	}
}

func TestParseParam(t *testing.T) {
	p, err := ParseParam("env=choice:staging,prod")
	if err != nil || string(p.Name) != "env" || p.Type != protocol.ActionParamType_Choice || p.Choices() == nil || len(p.Choices().Choices) != 2 {
		t.Fatalf("choice: %+v %v", p, err)
	}
	p, err = ParseParam("ref=token")
	if err != nil || string(p.Name) != "ref" || p.Type != protocol.ActionParamType_Token {
		t.Fatalf("token: %+v %v", p, err)
	}
	for _, bad := range []string{"env=choice:", "env=choice", "env=number", "Env=token", "=token", "env"} {
		if _, err := ParseParam(bad); err == nil {
			t.Errorf("ParseParam(%q) accepted", bad)
		}
	}
}

func TestValuesOrdersByParams(t *testing.T) {
	d := deploy()
	vs, err := Values(&d, map[string]string{"ref": "v1", "env": "staging"})
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(vs) != 2 || string(vs[0].Bytes) != "staging" || string(vs[1].Bytes) != "v1" {
		t.Fatalf("values = %+v, want env then ref", vs)
	}
	if _, err := Values(&d, map[string]string{"env": "staging"}); err == nil || !strings.Contains(err.Error(), "ref") {
		t.Errorf("missing ref: err = %v", err)
	}
	if _, err := Values(&d, map[string]string{"env": "staging", "ref": "v1", "x": "y"}); err == nil || !strings.Contains(err.Error(), "x") {
		t.Errorf("unknown x: err = %v", err)
	}
}

// A whole word {name} naming no declared param is a forgotten --param far more
// often than a literal: refused, pointing at the escape (final review I1).
// Stored as a literal, every grantee's --value would fail and a run with no
// values would pass "{env}" to the program, with nobody told.
func TestParseTemplateRefusesUndeclaredReference(t *testing.T) {
	_, err := ParseTemplate([]string{"./deploy.sh", "--env", "{env}"}, nil)
	if err == nil || !strings.Contains(err.Error(), "{env}") || !strings.Contains(err.Error(), `\{`) {
		t.Fatalf("undeclared {env}: err = %v, want a refusal naming it and the \\{ escape", err)
	}
	// Not a param-shaped name: a literal, as before ({} and {Bad} cannot be params).
	if _, err := ParseTemplate([]string{"echo", "{}", "{Bad}", "{a b}"}, nil); err != nil {
		t.Fatalf("non-param braces refused: %v", err)
	}
}
