package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// recordingSend is a send stream that keeps what is written to it.
type recordingSend struct {
	trsf.SendStream
	id    trsf.StreamID
	data  []byte
	ended bool
}

func (r *recordingSend) ID() trsf.StreamID { return r.id }
func (r *recordingSend) Close() error      { return nil }
func (r *recordingSend) AppendDataContext(_ context.Context, eof bool, data ...[]byte) error {
	for _, d := range data {
		r.data = append(r.data, d...)
	}
	r.ended = r.ended || eof
	return nil
}

func mustGrantCondition(t *testing.T, s string) protocol.GrantCondition {
	t.Helper()
	c, err := verb.ParseGrantCondition(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An open with a prompt and a grant condition names two streams: the prompt
// first, the conditions in the ext byte; the override the condition belongs to
// is marked, and the body travels on its own stream (grant conditions C6).
func TestOpenSendsGrantConditionsOnAStream(t *testing.T) {
	_, ov, err := verb.ParseScopeFor("forward_local=global")
	if err != nil {
		t.Fatal(err)
	}
	cond := mustGrantCondition(t, "forward_local=ports:8000-8100")
	opts := SessionOpts{Overrides: []protocol.ScopeOverride{ov}, GrantConditions: []protocol.GrantCondition{cond}, RecordSet: true, Record: true}
	oi := buildOpenInteractiveRequest("/r", opts)
	payloads, build, err := openBodies(oi, "hello", opts.GrantConditions)
	if err != nil {
		t.Fatal(err)
	}
	streams := []trsf.SendStream{&recordingSend{id: 3}, &recordingSend{id: 4}}
	var sent *protocol.TaskControlRequest
	begin := func(r *protocol.TaskControlRequest) (<-chan TaskControlResult, error) {
		sent = r
		ch := make(chan TaskControlResult, 1)
		ch <- TaskControlResult{Resp: &protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenInteractive}}
		return ch, nil
	}
	if _, err := taskControlWithPayloads(context.Background(), streams, begin, build, payloads); err != nil {
		t.Fatal(err)
	}
	got := sent.OpenInteractive()
	if got == nil || !got.PromptPresent() || *got.PromptStreamId() != 3 {
		t.Fatalf("prompt stream: %+v", got)
	}
	ext := got.Ext()
	if !got.ExtPresent() || ext == nil || !ext.GrantConditionsPresent() || *ext.GrantConditionsStreamId() != 4 || !ext.Record() {
		t.Fatalf("ext: %+v (the record switch must survive beside the conditions)", ext)
	}
	if len(got.Overrides) != 1 || !got.Overrides[0].Conditioned() {
		t.Fatalf("override not marked: %+v", got.Overrides)
	}
	var body protocol.GrantConditions
	if err := body.DecodeExact(streams[1].(*recordingSend).data); err != nil || len(body.Conditions) != 1 || body.Conditions[0].Caps != protocol.Capability_ForwardLocal {
		t.Fatalf("conditions stream: %+v %v", body, err)
	}
	var prompt protocol.PromptBody
	if err := prompt.DecodeExact(streams[0].(*recordingSend).data); err != nil || string(prompt.Prompt) != "hello" {
		t.Fatalf("prompt stream: %q %v", prompt.Prompt, err)
	}

	// No prompt, no conditions: no stream at all.
	if p, _, err := openBodies(buildOpenInteractiveRequest("/r", SessionOpts{}), "", nil); err != nil || len(p) != 0 {
		t.Fatalf("plain open: %d payloads, %v", len(p), err)
	}
	// A condition with no matching --scope-for is refused before anything is sent.
	if _, _, err := openBodies(buildOpenInteractiveRequest("/r", SessionOpts{}), "", []protocol.GrantCondition{cond}); err == nil || !strings.Contains(err.Error(), "--scope-for") {
		t.Fatalf("unmatched condition: %v", err)
	}
}

// payloadFake answers every request ok and keeps what a payload request sent.
type payloadFake struct {
	req     *protocol.TaskControlRequest
	payload []byte
	plain   int
}

func (f *payloadFake) RoundTripTaskControl(_ context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	f.plain++
	f.req = req
	resp := &protocol.TaskControlResponse{Kind: req.Header.Kind}
	resp.SetSetCaps(protocol.SetCapsResponse{})
	return resp, nil
}

func (f *payloadFake) TaskControlWithPayload(_ context.Context, build func(uint64) (*protocol.TaskControlRequest, error), payload []byte) (TaskControlResult, error) {
	req, err := build(9)
	if err != nil {
		return TaskControlResult{}, err
	}
	f.req, f.payload = req, payload
	resp := &protocol.TaskControlResponse{Kind: req.Header.Kind}
	resp.SetSetCaps(protocol.SetCapsResponse{})
	return TaskControlResult{Resp: resp}, nil
}

func TestSetCapsSendsGrantConditionsOnAStream(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local=global")
	scope := protocol.TaskScope{}
	f := &payloadFake{}
	_, err := SetCapsWith(context.Background(), f, SetCapsOpts{TaskID: strings.Repeat("ab", 16), Scope: &scope,
		Overrides: []protocol.ScopeOverride{ov}, GrantConditions: []protocol.GrantCondition{mustGrantCondition(t, "forward_local=ports:22")}})
	if err != nil {
		t.Fatal(err)
	}
	sc := f.req.SetCaps()
	if f.plain != 0 || sc == nil || sc.GrantConditionsStreamId != 9 || !sc.Overrides[0].Conditioned() {
		t.Fatalf("plain=%d request %+v", f.plain, sc)
	}
	var body protocol.GrantConditions
	if err := body.DecodeExact(f.payload); err != nil || len(body.Conditions) != 1 {
		t.Fatalf("body %+v %v", body, err)
	}
	// Without conditions it stays one datagram.
	f2 := &payloadFake{}
	if _, err := SetCapsWith(context.Background(), f2, SetCapsOpts{TaskID: strings.Repeat("ab", 16), Scope: &scope}); err != nil || f2.plain != 1 {
		t.Fatalf("plain set_caps: %v plain=%d", err, f2.plain)
	}
}

func TestCredentialIssueRequestCarriesGrantConditions(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local=global")
	a := verb.CredentialIssueAction{TTL: "1h", ScopePresent: true, Overrides: []protocol.ScopeOverride{ov},
		GrantConditions: []protocol.GrantCondition{mustGrantCondition(t, "forward_local=ports:22")}}
	req, body, err := credentialIssueRequest(a, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	is := req.CredentialIssue()
	if is == nil || !is.Overrides[0].Conditioned() || body == nil {
		t.Fatalf("request %+v body %v", is, body)
	}
	setGrantConditionsStream(req, 4)
	if req.CredentialIssue().GrantConditionsStreamId != 4 {
		t.Fatal("stream id not set on the issue")
	}
	a.Sub, a.ID = verb.SubRekey, strings.Repeat("cd", 16)
	rk, _, err := credentialIssueRequest(a, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	setGrantConditionsStream(rk, 5)
	if r := rk.CredentialRekey(); r == nil || !r.Overrides[0].Conditioned() || r.GrantConditionsStreamId != 5 {
		t.Fatalf("rekey %+v", r)
	}
}

func TestScheduleSendsGrantConditionsOnAStream(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local=global")
	o := ScheduleSpecOpts{Prompt: "p", ScopePresent: true, Overrides: []protocol.ScopeOverride{ov},
		GrantConditions: []protocol.GrantCondition{mustGrantCondition(t, "forward_local=ports:22")}}
	payloads, build, err := scheduleBodies(o, func(o ScheduleSpecOpts, promptID uint64) *protocol.TaskControlRequest {
		return scheduleCreateRequest(o, promptID)
	})
	if err != nil || len(payloads) != 2 {
		t.Fatalf("%d payloads, %v", len(payloads), err)
	}
	req := build([]uint64{3, 4})
	spec := req.ScheduleCreate().Spec
	if id := spec.PromptStreamId(); id == nil || *id != 3 || spec.GrantConditionsStreamId != 4 {
		t.Fatalf("stream ids: prompt %v conditions %d", spec.PromptStreamId(), spec.GrantConditionsStreamId)
	}
	if ovs := spec.Overrides(); ovs == nil || !(*ovs)[0].Conditioned() {
		t.Fatalf("override not marked: %+v", spec.Overrides())
	}
}

// The ls line and its JSON show the condition beside the scope; a forward_local
// condition does not read as covering forward_remote (Review Focus 5).
func TestListShowsGrantConditions(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local=ids:" + strings.Repeat("ab", 16))
	cond := mustGrantCondition(t, "forward_local=ports:8000-8100")
	marked, _ := verb.MarkGrantConditions([]protocol.ScopeOverride{ov}, []protocol.GrantCondition{cond})
	ti := protocol.TaskInfo{Capabilities: protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote}
	ti.SetOverrides(marked)
	ti.SetGrantConditions([]protocol.GrantCondition{cond})
	line := taskLine(ti, nil)
	if !strings.Contains(line, "conditions=forward_local:ports:8000-8100") {
		t.Fatalf("ls line: %s", line)
	}
	j := newTaskJSON(&ti, nil)
	if len(j.ConditionsByCap) != 1 || j.ConditionsByCap["forward_local"] != "ports:8000-8100" {
		t.Fatalf("conditions_by_cap = %v", j.ConditionsByCap)
	}
	// The marker without its body: shown as unknown, never dropped.
	ti.SetGrantConditions(nil)
	if line := taskLine(ti, nil); !strings.Contains(line, "conditions=forward_local:(condition unknown)") {
		t.Fatalf("ls line without body: %s", line)
	}
}

// whoami's inline answer carries the marker only (grant conditions C6): shown
// as unknown until the bodies are fetched through the list path.
func TestWhoAmIShowsGrantConditions(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local=global")
	cond := mustGrantCondition(t, "forward_local=ports:22")
	marked, _ := verb.MarkGrantConditions([]protocol.ScopeOverride{ov}, []protocol.GrantCondition{cond})
	resp := protocol.WhoAmIResponse{PrincipalTaskId: protocol.TaskID{Id: [16]byte{1}}, Capabilities: protocol.Capability_ForwardLocal}
	resp.SetOverrides(marked)
	var text, js, with strings.Builder
	if err := WriteWhoAmI(&text, resp, false); err != nil || !strings.Contains(text.String(), "conditions=forward_local:(condition unknown)") {
		t.Fatalf("text: %q %v", text.String(), err)
	}
	if err := WriteWhoAmI(&js, resp, true); err != nil || !strings.Contains(js.String(), `"conditions_by_cap":{"forward_local":"(condition unknown)"}`) {
		t.Fatalf("json: %q %v", js.String(), err)
	}
	if err := WriteWhoAmIWith(&with, resp, []protocol.GrantCondition{cond}, false); err != nil || !strings.Contains(with.String(), "conditions=forward_local:ports:22") {
		t.Fatalf("with bodies: %q %v", with.String(), err)
	}
}

// caps audit's scope cell is what every surface prints for a holder; a
// conditioned holder must not read as an unconditioned one there (anti-goal 1).
func TestCapsAuditShowsGrantCondition(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local=global")
	cond := mustGrantCondition(t, "forward_local=ports:22")
	marked, _ := verb.MarkGrantConditions([]protocol.ScopeOverride{ov}, []protocol.GrantCondition{cond})
	ti := protocol.TaskInfo{Id: protocol.TaskID{Id: [16]byte{7}}, Status: protocol.TaskStatus_Running,
		Capabilities: protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote}
	ti.SetOverrides(marked)
	ti.SetGrantConditions([]protocol.GrantCondition{cond})
	var local, remote string
	for _, r := range AuditRowsJSON(CapsAudit([]protocol.TaskInfo{ti}, false, false)) {
		for _, h := range r.Holders {
			switch r.Cap {
			case "forward_local":
				local = h.ScopeText
			case "forward_remote":
				remote = h.ScopeText
			}
		}
	}
	if !strings.Contains(local, "if ports:22") || strings.Contains(remote, "if ") {
		t.Fatalf("forward_local %q, forward_remote %q", local, remote)
	}
}

// The dialogs send a task's conditions back beside its overrides; a marker
// whose body is unknown renders a spec that does not parse, so the re-grant is
// refused rather than sent unconditioned.
func TestGrantConditionSpecsRoundTrip(t *testing.T) {
	_, ov, _ := verb.ParseScopeFor("forward_local,forward_remote=global")
	_, plain, _ := verb.ParseScopeFor("cancel=none")
	cond := mustGrantCondition(t, "forward_local,forward_remote=ports:22,8000-8100")
	marked, _ := verb.MarkGrantConditions([]protocol.ScopeOverride{plain, ov}, []protocol.GrantCondition{cond})
	specs := GrantConditionSpecs(marked, []protocol.GrantCondition{cond})
	if len(specs) != 1 || specs[0] != "forward_local,forward_remote=ports:22,8000-8100" {
		t.Fatalf("specs = %v", specs)
	}
	back, err := verb.ParseGrantConditionList(specs)
	if err != nil || len(back) != 1 || back[0].Caps != cond.Caps {
		t.Fatalf("parse back: %v %v", back, err)
	}
	unknown := GrantConditionSpecs(marked, nil)
	if _, err := verb.ParseGrantConditionList(unknown); err == nil {
		t.Fatalf("an unknown body %v parsed; a re-grant would send it unconditioned", unknown)
	}
}
