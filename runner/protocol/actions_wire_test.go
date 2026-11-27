package protocol

import "testing"

// Actions (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C8, C12, C16, C17): definitions written on a stream, an invocation riding
// the exec body, the action shown on the exec row.

func lit(t *testing.T, s string) ActionLiteral {
	t.Helper()
	var l ActionLiteral
	if !l.SetBytes([]byte(s)) {
		t.Fatalf("SetBytes(%q) refused", s)
	}
	return l
}

func litElem(t *testing.T, s string) ActionElement {
	t.Helper()
	e := ActionElement{Kind: ActionElementKind_Literal}
	if !e.SetLiteral(lit(t, s)) {
		t.Fatal("SetLiteral refused")
	}
	return e
}

func paramElem(t *testing.T, i uint8) ActionElement {
	t.Helper()
	e := ActionElement{Kind: ActionElementKind_Param}
	if !e.SetParamIndex(i) {
		t.Fatal("SetParamIndex refused")
	}
	return e
}

// sampleDefinition is deploy: ./deploy.sh --env {env} --ref {ref}.
func sampleDefinition(t *testing.T) ActionDefinition {
	t.Helper()
	var def ActionDefinition
	def.SetName([]byte("deploy"))
	env := ActionParam{Type: ActionParamType_Choice}
	env.SetName([]byte("env"))
	var ch ActionChoices
	if !ch.SetChoices([]ActionLiteral{lit(t, "staging"), lit(t, "prod")}) {
		t.Fatal("SetChoices refused")
	}
	if !env.SetChoices(ch) {
		t.Fatal("ActionParam.SetChoices refused")
	}
	ref := ActionParam{Type: ActionParamType_Token}
	ref.SetName([]byte("ref"))
	if !def.SetParams([]ActionParam{env, ref}) {
		t.Fatal("SetParams refused")
	}
	if !def.SetTemplate([]ActionElement{litElem(t, "./deploy.sh"), litElem(t, "--env"), paramElem(t, 0), litElem(t, "--ref"), paramElem(t, 1)}) {
		t.Fatal("SetTemplate refused")
	}
	def.SetDescription([]byte("deploy a ref"))
	return def
}

func TestActionDefinitionRoundTrip(t *testing.T) {
	def := sampleDefinition(t)
	buf, err := def.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got ActionDefinition
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Name) != "deploy" || string(got.Description) != "deploy a ref" || len(got.Params) != 2 || len(got.Template) != 5 {
		t.Fatalf("definition = %+v", got)
	}
	env := got.Params[0]
	if string(env.Name) != "env" || env.Type != ActionParamType_Choice || env.Choices() == nil || len(env.Choices().Choices) != 2 ||
		string(env.Choices().Choices[1].Bytes) != "prod" {
		t.Fatalf("param 0 = %+v", env)
	}
	if got.Params[1].Type != ActionParamType_Token || got.Params[1].Choices() != nil {
		t.Fatalf("param 1 = %+v, want a token with no choices", got.Params[1])
	}
	if got.Template[0].Literal() == nil || string(got.Template[0].Literal().Bytes) != "./deploy.sh" {
		t.Fatalf("element 0 = %+v", got.Template[0])
	}
	if p := got.Template[4].ParamIndex(); p == nil || *p != 1 {
		t.Fatalf("element 4 = %+v, want param 1", got.Template[4])
	}
}

func TestActionPutRequestResponseRoundTrip(t *testing.T) {
	req := TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ActionPut, RequestId: 7}}
	req.SetActionPut(ActionPutRequest{Id: actionID(3), ExpectRevision: 2, PayloadStreamId: 9})
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append request: %v", err)
	}
	var gotReq TaskControlRequest
	if err := gotReq.DecodeExact(buf); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if p := gotReq.ActionPut(); p == nil || p.Id != actionID(3) || p.ExpectRevision != 2 || p.PayloadStreamId != 9 {
		t.Fatalf("request = %+v", gotReq.ActionPut())
	}

	resp := TaskControlResponse{Kind: TaskControlKind_ActionPut, RequestId: 7}
	body := ActionPutResponse{Status: ActionStatus_HoldersUnreachable, Id: actionID(3), Revision: 2, HoldersRefused: 4}
	body.SetMessage([]byte("4 holders outside your reach"))
	resp.SetActionPut(body)
	buf, err = resp.Append(nil)
	if err != nil {
		t.Fatalf("append response: %v", err)
	}
	var gotResp TaskControlResponse
	if err := gotResp.DecodeExact(buf); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	r := gotResp.ActionPut()
	if r == nil || r.Status != ActionStatus_HoldersUnreachable || r.HoldersRefused != 4 || string(r.Message) != "4 holders outside your reach" {
		t.Fatalf("response = %+v", r)
	}

	rm := TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ActionRemove}}
	rm.SetActionRemove(ActionRemoveRequest{Id: actionID(5)})
	if _, err := rm.Append(nil); err != nil {
		t.Fatalf("append remove: %v", err)
	}
	ls := TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ActionList}}
	ls.SetActionList(ActionListRequest{})
	if _, err := ls.Append(nil); err != nil {
		t.Fatalf("append list: %v", err)
	}
}

func TestActionListBodyRoundTrip(t *testing.T) {
	rec := ActionRecord{Id: actionID(1), Revision: 3, Definition: sampleDefinition(t), CreatedAtMs: 10, EditedAtMs: 20}
	rec.SetCreatedBy([]byte("operator"))
	rec.SetEditedBy([]byte("task:00112233445566778899aabbccddeeff"))
	var body ActionListBody
	if !body.SetActions([]ActionRecord{rec}) {
		t.Fatal("SetActions refused")
	}
	buf, err := body.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got ActionListBody
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Actions) != 1 || got.Actions[0].Revision != 3 || string(got.Actions[0].EditedBy) != "task:00112233445566778899aabbccddeeff" ||
		string(got.Actions[0].Definition.Name) != "deploy" {
		t.Fatalf("body = %+v", got)
	}
}

func TestExecRunBodyActionVariantRoundTrip(t *testing.T) {
	inv := ActionInvocation{Id: actionID(2)}
	var v ActionValue
	v.SetBytes([]byte("prod"))
	if !inv.SetValues([]ActionValue{v}) {
		t.Fatal("SetValues refused")
	}
	var body ExecRunBody
	body.SetActionPresent(true)
	body.SetAction(inv)
	buf, err := body.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got ExecRunBody
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.ActionPresent() || got.Action() == nil || got.Action().Id != actionID(2) || string(got.Action().Values[0].Bytes) != "prod" {
		t.Fatalf("body = %+v", got)
	}
	if got.Argv.ArgvLen != 0 {
		t.Fatalf("argv = %d elements, want none", got.Argv.ArgvLen)
	}

	var plain ExecRunBody
	buf, _ = plain.Append(nil)
	var gotPlain ExecRunBody
	if err := gotPlain.DecodeExact(buf); err != nil || gotPlain.ActionPresent() || gotPlain.Action() != nil {
		t.Fatalf("plain body: err=%v %+v", err, gotPlain)
	}
}

func TestExecRunInfoActionFields(t *testing.T) {
	var info ExecRunInfo
	info.SetPty(true)
	info.SetActionPresent(true)
	info.SetActionId(actionID(4))
	info.SetActionRevision(6)
	info.SetActionName([]byte("deploy"))
	buf, err := info.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got ExecRunInfo
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Pty() || !got.ActionPresent() || got.ActionId() == nil || *got.ActionId() != actionID(4) ||
		got.ActionRevision() == nil || *got.ActionRevision() != 6 || got.ActionName() == nil || string(*got.ActionName()) != "deploy" {
		t.Fatalf("info = %+v", got)
	}
}

func TestCapabilityAllIncludesActionAdmin(t *testing.T) {
	if Capability_ActionAdmin != 0x100000000 {
		t.Fatalf("action_admin = %#x", uint64(Capability_ActionAdmin))
	}
	if Capability_All&Capability_ActionAdmin == 0 {
		t.Fatal("all lacks action_admin: the operator would be denied its own feature")
	}
	if uint64(0xffffffff)&uint64(Capability_ActionAdmin) != 0 {
		t.Fatal("old all grants gained action_admin")
	}
}

func TestExecRunStatusActionValues(t *testing.T) {
	// Appended after bad_body, so no existing ordinal moves.
	if ExecRunStatus_NoSuchAction != ExecRunStatus_BadBody+1 || ExecRunStatus_OutsideCondition != ExecRunStatus_BadBody+2 ||
		ExecRunStatus_BadActionValues != ExecRunStatus_BadBody+3 {
		t.Fatalf("statuses: %d %d %d after bad_body %d", ExecRunStatus_NoSuchAction, ExecRunStatus_OutsideCondition, ExecRunStatus_BadActionValues, ExecRunStatus_BadBody)
	}
}
