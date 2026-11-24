package server

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Task 5 of the credential-tasks plan: the issue and re-key verbs, the
// refusals that keep a credential out of every runner path, and what whoami
// and the list report about one.

var credConnSeq atomic.Int32

// credTestConn is a fresh connection with no principal: an operator unless a
// principal is recorded for it.
func credTestConn() *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%d-1", 9400+credConnSeq.Add(1))), autoSendStreams: true}
}

// credAgent makes an operator-created task holding g and an agent connection
// whose principal it is.
func credAgent(t *testing.T, h *TaskHandler, g authority.Grant) (*fakeConn, string) {
	t.Helper()
	id := h.Tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, g, "claude")
	conn := credTestConn()
	cid := conn.ConnectionID()
	h.clientKindsMu.Lock()
	if h.principals == nil {
		h.principals = map[objproto.ConnectionID]protocol.TaskID{}
	}
	if h.clientKinds == nil {
		h.clientKinds = map[objproto.ConnectionID]protocol.ClientKind{}
	}
	h.principals[cid] = taskIDFromHex(id)
	h.clientKinds[cid] = protocol.ClientKind_Agent
	h.clientKindsMu.Unlock()
	return conn, id
}

func plainTask(h *TaskHandler) string {
	return h.Tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "claude")
}

func ttlMs(d time.Duration) uint64 { return uint64(d / time.Millisecond) }

func issueRequest(pub [32]byte, ttl uint64, caps protocol.Capability, scope authority.Scope) protocol.CredentialIssueRequest {
	r := protocol.CredentialIssueRequest{Pubkey: pub, TtlMs: ttl, RequestedCaps: caps, Scope: scope.ToWire()}
	r.SetOverrides(scope.OverridesToWire())
	r.SetName([]byte("ci"))
	return r
}

func rekeyRequest(id string, pub [32]byte, ttl uint64) protocol.CredentialRekeyRequest {
	return protocol.CredentialRekeyRequest{TaskId: taskIDFromHex(id), Pubkey: pub, TtlMs: ttl}
}

// credCall sends a credential request on conn and returns the credential
// answer, or nil and the response when it was something else (a denial).
func credCall(t *testing.T, h *TaskHandler, conn *fakeConn, fill func(*protocol.TaskControlRequest)) (*protocol.CredentialResponse, protocol.TaskControlResponse) {
	t.Helper()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{RequestId: 5}}
	fill(req)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	switch resp.Kind {
	case protocol.TaskControlKind_CredentialIssue:
		return resp.CredentialIssue(), resp
	case protocol.TaskControlKind_CredentialRekey:
		return resp.CredentialRekey(), resp
	}
	return nil, resp
}

func issue(t *testing.T, h *TaskHandler, conn *fakeConn, r protocol.CredentialIssueRequest) (*protocol.CredentialResponse, protocol.TaskControlResponse) {
	t.Helper()
	return credCall(t, h, conn, func(req *protocol.TaskControlRequest) {
		req.Header.Kind = protocol.TaskControlKind_CredentialIssue
		req.SetCredentialIssue(r)
	})
}

func rekey(t *testing.T, h *TaskHandler, conn *fakeConn, r protocol.CredentialRekeyRequest) (*protocol.CredentialResponse, protocol.TaskControlResponse) {
	t.Helper()
	return credCall(t, h, conn, func(req *protocol.TaskControlRequest) {
		req.Header.Kind = protocol.TaskControlKind_CredentialRekey
		req.SetCredentialRekey(r)
	})
}

func wantCredStatus(t *testing.T, what string, r *protocol.CredentialResponse, resp protocol.TaskControlResponse, want protocol.CredentialStatus) {
	t.Helper()
	if r == nil {
		t.Fatalf("%s: answered %v, want credential status %v", what, resp.Kind, want)
	}
	if r.Status != want {
		t.Fatalf("%s: status %v (%s), want %v", what, r.Status, r.Message, want)
	}
}

func wantDeniedFor(t *testing.T, what string, r *protocol.CredentialResponse, resp protocol.TaskControlResponse, want protocol.Capability) {
	t.Helper()
	pd := resp.PermissionDenied()
	if r != nil || resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil || pd.RequiredCap != want {
		t.Fatalf("%s: answered %v %+v, want permission_denied for %v", what, resp.Kind, r, want)
	}
}

// --- issue ---

func TestCredentialIssueNeedsIssueBit(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	agent, _ := credAgent(t, h, authority.Grant{Caps: protocol.Capability_Spawn | protocol.Capability_ScheduleRun,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global}, DelegateCaps: protocol.Capability_All})
	before := len(h.Tasks.List(0))
	r, resp := issue(t, h, agent, issueRequest(pub, ttlMs(time.Hour), protocol.Capability_ScheduleRun, authority.Scope{}))
	wantDeniedFor(t, "agent without credential_issue", r, resp, protocol.Capability_CredentialIssue)
	if n := len(h.Tasks.List(0)); n != before {
		t.Fatalf("a denied issue created %d tasks", n-before)
	}

	op := credTestConn()
	start := time.Now()
	r, resp = issue(t, h, op, issueRequest(pub, ttlMs(time.Hour), protocol.Capability_ScheduleRun, authority.Scope{}))
	wantCredStatus(t, "operator", r, resp, protocol.CredentialStatus_Ok)
	id := hex.EncodeToString(r.TaskId.Id[:])
	rec, ok := h.Tasks.Get(id)
	if !ok || rec.Kind != protocol.TaskKind_Credential || rec.Status != protocol.TaskStatus_Active {
		t.Fatalf("issued row %+v ok=%v", rec, ok)
	}
	if rec.CredentialPubKey != pub || rec.Name != "ci" || rec.CreatorTaskID != (protocol.TaskID{}) {
		t.Fatalf("issued row key/name/creator %x %q %x", rec.CredentialPubKey, rec.Name, rec.CreatorTaskID.Id)
	}
	if rec.ExpiresAt.Before(start.Add(time.Hour)) || rec.ExpiresAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("expiry %v is not now + 1h on the server clock", rec.ExpiresAt)
	}
	if r.ExpiresAt != uint64(rec.ExpiresAt.UnixNano()) {
		t.Fatalf("response expiry %d, row %d", r.ExpiresAt, rec.ExpiresAt.UnixNano())
	}
}

// D13: the issued task is a child of the issuer holding at most what it holds,
// inside its scope; a bit it lacks is dropped, an id outside its reach refused.
func TestCredentialIssueAttenuatesToIssuer(t *testing.T) {
	h := newTestHandler(t)
	target := plainTask(h)
	other := plainTask(h)
	_, pub := newCredentialKey(t)
	onT := authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{target}}
	agent, agentID := credAgent(t, h, authority.Grant{
		Caps:         protocol.Capability_ScheduleRun | protocol.Capability_ResumeExact | protocol.Capability_CredentialIssue,
		Scope:        onT,
		DelegateCaps: protocol.Capability_All, DelegateScope: onT,
	})
	want := protocol.Capability_ScheduleRun | protocol.Capability_ResumeExact
	r, resp := issue(t, h, agent, issueRequest(pub, ttlMs(time.Hour), want, onT))
	wantCredStatus(t, "within the issuer", r, resp, protocol.CredentialStatus_Ok)
	rec, _ := h.Tasks.Get(hex.EncodeToString(r.TaskId.Id[:]))
	if rec.Capabilities != want || hex.EncodeToString(rec.CreatorTaskID.Id[:]) != agentID {
		t.Fatalf("caps %v creator %x, want %v under the issuer", rec.Capabilities, rec.CreatorTaskID.Id, want)
	}
	if rec.OriginKind != protocol.ClientKind_Agent {
		t.Fatalf("origin kind %v, want agent", rec.OriginKind)
	}
	if len(rec.Scope.IDs) != 1 || rec.Scope.IDs[0] != target {
		t.Fatalf("scope %v, want the issuer's target", rec.Scope)
	}

	r, resp = issue(t, h, agent, issueRequest(pub, ttlMs(time.Hour), want|protocol.Capability_Spawn, onT))
	wantCredStatus(t, "asking for spawn it lacks", r, resp, protocol.CredentialStatus_Ok)
	if rec, _ := h.Tasks.Get(hex.EncodeToString(r.TaskId.Id[:])); rec.Capabilities&protocol.Capability_Spawn != 0 {
		t.Fatalf("caps %v hold spawn the issuer lacks", rec.Capabilities)
	}

	r, resp = issue(t, h, agent, issueRequest(pub, ttlMs(time.Hour), want, authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{other}}))
	wantCredStatus(t, "an id outside the issuer's reach", r, resp, protocol.CredentialStatus_ScopeNotPermitted)
}

// Review Focus 5: no TTL and one past the bound are refused; the bound itself
// is accepted. The expiry is the server's clock plus a duration.
func TestCredentialIssueTTLBounds(t *testing.T) {
	h := newTestHandler(t)
	op := credTestConn()
	_, pub := newCredentialKey(t)
	for _, c := range []struct {
		name string
		ttl  uint64
		want protocol.CredentialStatus
	}{
		{"zero", 0, protocol.CredentialStatus_TtlInvalid},
		{"bound + 1ms", ttlMs(CredentialMaxTTL) + 1, protocol.CredentialStatus_TtlTooLong},
		{"u64 max (no overflow into a short ttl)", ^uint64(0), protocol.CredentialStatus_TtlTooLong},
		{"the bound", ttlMs(CredentialMaxTTL), protocol.CredentialStatus_Ok},
	} {
		r, resp := issue(t, h, op, issueRequest(pub, c.ttl, protocol.Capability_ScheduleRun, authority.Scope{}))
		wantCredStatus(t, c.name, r, resp, c.want)
		if c.want != protocol.CredentialStatus_Ok && !strings.Contains(string(r.Message), "90") && c.ttl != 0 {
			t.Errorf("%s: message %q does not name the bound", c.name, r.Message)
		}
	}
	creds := 0
	for _, e := range h.Tasks.List(0) {
		if e.Kind == protocol.TaskKind_Credential {
			creds++
		}
	}
	if creds != 1 {
		t.Fatalf("%d credentials issued, want 1 (only the accepted ttl)", creds)
	}
}

// D14: a grant naming a masked bit is refused, not silently masked.
func TestCredentialIssueRefusesMaskedCaps(t *testing.T) {
	h := newTestHandler(t)
	op := credTestConn()
	_, pub := newCredentialKey(t)
	for name, mk := range map[string]func() protocol.CredentialIssueRequest{
		"all": func() protocol.CredentialIssueRequest {
			return issueRequest(pub, ttlMs(time.Hour), protocol.Capability_All, authority.Scope{})
		},
		"credential_issue": func() protocol.CredentialIssueRequest {
			return issueRequest(pub, ttlMs(time.Hour), protocol.Capability_CredentialIssue, authority.Scope{})
		},
		"schedule_edit": func() protocol.CredentialIssueRequest {
			return issueRequest(pub, ttlMs(time.Hour), protocol.Capability_ScheduleEdit, authority.Scope{})
		},
		"delegate credential_issue": func() protocol.CredentialIssueRequest {
			r := issueRequest(pub, ttlMs(time.Hour), protocol.Capability_ScheduleRun, authority.Scope{})
			r.DelegateCaps = protocol.Capability_CredentialIssue
			return r
		},
	} {
		r, resp := issue(t, h, op, mk())
		wantCredStatus(t, name, r, resp, protocol.CredentialStatus_MaskedCap)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("refused issues created %d tasks", n)
	}
}

// Carried item: every small-order encoding (and an off-curve one) is
// bad_pubkey on both issue and re-key. ed25519.Verify accepts forged
// signatures under a small-order key.
func TestCredentialIssueBadPubkey(t *testing.T) {
	h := newTestHandler(t)
	op := credTestConn()
	_, good := newCredentialKey(t)
	r, resp := issue(t, h, op, issueRequest(good, ttlMs(time.Hour), protocol.Capability_ScheduleRun, authority.Scope{}))
	wantCredStatus(t, "a generated key", r, resp, protocol.CredentialStatus_Ok)
	cred := hex.EncodeToString(r.TaskId.Id[:])

	var bad [][32]byte
	for _, e := range smallOrderEncodings {
		bad = append(bad, withSign(e, 0), withSign(e, 1))
	}
	var offCurve [32]byte
	for y := byte(2); ; y++ {
		offCurve = [32]byte{y}
		if _, ok := edDecode(offCurve); !ok {
			break
		}
	}
	bad = append(bad, offCurve)
	for i, k := range bad {
		r, resp := issue(t, h, op, issueRequest(k, ttlMs(time.Hour), protocol.Capability_ScheduleRun, authority.Scope{}))
		wantCredStatus(t, fmt.Sprintf("issue with bad key %d (%x)", i, k), r, resp, protocol.CredentialStatus_BadPubkey)
		r, resp = rekey(t, h, op, rekeyRequest(cred, k, ttlMs(time.Hour)))
		wantCredStatus(t, fmt.Sprintf("rekey with bad key %d (%x)", i, k), r, resp, protocol.CredentialStatus_BadPubkey)
	}
	if rec, _ := h.Tasks.Get(cred); rec.CredentialPubKey != good {
		t.Fatal("a refused re-key replaced the key")
	}
}

// --- re-key (D18) ---

// credUnder stores a credential whose creator is creatorHex, holding g, as an
// issue by that task would have (the store, so a test can give the creator
// less than the credential holds).
func credUnder(t *testing.T, h *TaskHandler, creatorHex string, g authority.Grant, pub [32]byte) string {
	t.Helper()
	id, err := h.Tasks.CreateCredential(taskIDFromHex(creatorHex), protocol.ClientKind_Agent, g, pub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCredentialRekeyGatedAsIssue(t *testing.T) {
	h := newTestHandler(t)
	var dropped []string
	h.DropStaleCredentialConns = func(id string) int { dropped = append(dropped, id); return 1 }
	target := plainTask(h)
	op := credTestConn()
	_, oldPub := newCredentialKey(t)
	_, newPub := newCredentialKey(t)
	held := protocol.Capability_ScheduleRun | protocol.Capability_ResumeExact
	onT := authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{target}}
	global := authority.Scope{Base: protocol.ScopeBase_Global}

	// The issuer, through the issue verb: its credential is its child.
	issuer, _ := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | held,
		Scope: global, DelegateCaps: protocol.Capability_All, DelegateScope: global})
	r, resp := issue(t, h, issuer, issueRequest(oldPub, ttlMs(time.Hour), held, onT))
	wantCredStatus(t, "issue", r, resp, protocol.CredentialStatus_Ok)
	cred := hex.EncodeToString(r.TaskId.Id[:])
	oldConn := recordCredentialConn(t, h, taskIDFromHex(cred), oldPub, 9500)

	// resume is not the power: re-key is gated as issue.
	resumer, _ := credAgent(t, h, authority.Grant{Caps: protocol.Capability_Resume | protocol.Capability_ResumeExact | protocol.Capability_ScheduleRun,
		Scope: global, DelegateCaps: protocol.Capability_All, DelegateScope: global})
	r, resp = rekey(t, h, resumer, rekeyRequest(cred, newPub, ttlMs(time.Hour)))
	wantDeniedFor(t, "resume without credential_issue", r, resp, protocol.Capability_CredentialIssue)

	// Issue-equivalence, caps: an ancestor whose credential holds resume_exact,
	// which the ancestor itself does not.
	narrowCaps, narrowCapsID := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | protocol.Capability_ScheduleRun,
		Scope: global, DelegateCaps: protocol.Capability_All, DelegateScope: global})
	c1 := credUnder(t, h, narrowCapsID, authority.Grant{Caps: held, Scope: onT}, oldPub)
	r, resp = rekey(t, h, narrowCaps, rekeyRequest(c1, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "caller lacks a bit the credential holds", r, resp, protocol.CredentialStatus_ScopeNotPermitted)

	// Issue-equivalence, scope: an ancestor that reaches its credential but
	// not the credential's target.
	narrowScope, narrowScopeID := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | held,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: protocol.Capability_All, DelegateScope: global})
	c2 := credUnder(t, h, narrowScopeID, authority.Grant{Caps: held, Scope: onT}, oldPub)
	r, resp = rekey(t, h, narrowScope, rekeyRequest(c2, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "caller's scope misses the credential's target", r, resp, protocol.CredentialStatus_ScopeNotPermitted)

	// Its own credential outside its credential_issue scope is no such task.
	outOfReach, outOfReachID := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | held,
		Scope: authority.Scope{Base: protocol.ScopeBase_None}, DelegateCaps: protocol.Capability_All})
	c3 := credUnder(t, h, outOfReachID, authority.Grant{Caps: held, Scope: onT}, oldPub)
	r, resp = rekey(t, h, outOfReach, rekeyRequest(c3, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "credential out of reach", r, resp, protocol.CredentialStatus_NoSuchTask)

	r, resp = rekey(t, h, op, rekeyRequest(target, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "a task that is not a credential", r, resp, protocol.CredentialStatus_NotCredential)
	r, resp = rekey(t, h, op, rekeyRequest(strings.Repeat("ab", 16), newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "an unknown id", r, resp, protocol.CredentialStatus_NoSuchTask)
	r, resp = rekey(t, h, op, rekeyRequest(cred, oldPub, ttlMs(time.Hour)))
	wantCredStatus(t, "the key it already holds", r, resp, protocol.CredentialStatus_BadPubkey)
	r, resp = rekey(t, h, op, rekeyRequest(cred, newPub, 0))
	wantCredStatus(t, "no expiry", r, resp, protocol.CredentialStatus_TtlInvalid)
	if len(dropped) != 0 {
		t.Fatalf("a refused re-key closed connections: %v", dropped)
	}
	if rec, _ := h.Tasks.Get(cred); rec.CredentialPubKey != oldPub {
		t.Fatal("a refused re-key replaced the key")
	}

	// The issuer, an ancestor that could issue the same credential now, may
	// re-key it.
	start := time.Now()
	r, resp = rekey(t, h, issuer, rekeyRequest(cred, newPub, ttlMs(2*time.Hour)))
	wantCredStatus(t, "the issuer", r, resp, protocol.CredentialStatus_Ok)
	rec, _ := h.Tasks.Get(cred)
	if rec.CredentialPubKey != newPub || rec.ExpiresAt.Before(start.Add(2*time.Hour)) || rec.Capabilities != held {
		t.Fatalf("after re-key: key %x expiry %v caps %v", rec.CredentialPubKey, rec.ExpiresAt, rec.Capabilities)
	}
	if hex.EncodeToString(r.TaskId.Id[:]) != cred || r.ExpiresAt != uint64(rec.ExpiresAt.UnixNano()) {
		t.Fatalf("response id %x expiry %d", r.TaskId.Id, r.ExpiresAt)
	}
	if len(dropped) != 1 || dropped[0] != cred {
		t.Fatalf("connections dropped for %v, want the credential's", dropped)
	}
	if caps := h.callerCaps(oldConn.ConnectionID()); caps != protocol.Capability_None {
		t.Fatalf("the old key's connection still holds %v", caps)
	}

	// The operator re-keys a revoked credential it did not issue, narrowing it
	// on the way.
	h.Tasks.Cancel(cred, protocol.EndReason_Cancelled, protocol.TaskID{})
	_, thirdPub := newCredentialKey(t)
	rk := rekeyRequest(cred, thirdPub, ttlMs(time.Hour))
	rk.SetCapsPresent(true)
	rk.RequestedCaps = protocol.Capability_ScheduleRun
	r, resp = rekey(t, h, op, rk)
	wantCredStatus(t, "operator re-key of a revoked credential", r, resp, protocol.CredentialStatus_Ok)
	if rec, _ := h.Tasks.Get(cred); rec.Status != protocol.TaskStatus_Active || rec.Capabilities != protocol.Capability_ScheduleRun {
		t.Fatalf("after the operator's re-key: status %v caps %v", rec.Status, rec.Capabilities)
	}

	// A narrowing that names a masked bit is refused like an issue's.
	_, fourthPub := newCredentialKey(t)
	rk = rekeyRequest(cred, fourthPub, ttlMs(time.Hour))
	rk.SetCapsPresent(true)
	rk.RequestedCaps = protocol.Capability_ScheduleEdit
	r, resp = rekey(t, h, op, rk)
	wantCredStatus(t, "re-key naming schedule_edit", r, resp, protocol.CredentialStatus_MaskedCap)
}

// D18, the reviewer's probe: a caller that reaches an operator-issued
// credential C only by an id in its scope is not its creator-tree ancestor.
// C's subtree scope is relative to C, so attenuation passes it, and the new
// key's holder would reach C's child D, which the caller cannot. Refused as
// out of reach; once the operator reparents C under the caller, it works.
func TestCredentialRekeyByNonAncestorRefused(t *testing.T) {
	h := newTestHandler(t)
	op := credTestConn()
	_, pub := newCredentialKey(t)
	bits := protocol.Capability_Cancel | protocol.Capability_FileRead
	r, resp := issue(t, h, op, issueRequest(pub, ttlMs(time.Hour), bits, authority.Scope{Base: protocol.ScopeBase_Subtree}))
	wantCredStatus(t, "operator issue", r, resp, protocol.CredentialStatus_Ok)
	cred := hex.EncodeToString(r.TaskId.Id[:])
	child := h.Tasks.Create("/r", "d", protocol.TaskKind_Interactive, protocol.ClientKind_Credential, taskIDFromHex(cred), "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "claude")

	g, gID := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | bits,
		Scope:        authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{cred}},
		DelegateCaps: protocol.Capability_All, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}})
	gCID := g.ConnectionID()
	if !h.inScope(gCID, protocol.Capability_CredentialIssue, cred) {
		t.Fatal("setup: the caller does not reach the credential through its scope")
	}
	for _, bit := range []protocol.Capability{protocol.Capability_Cancel, protocol.Capability_FileRead} {
		if h.inScope(gCID, bit, child) {
			t.Fatalf("setup: the caller reaches C's child for %v on its own", bit)
		}
	}

	_, newPub := newCredentialKey(t)
	r, resp = rekey(t, h, g, rekeyRequest(cred, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "re-key by a non-ancestor", r, resp, protocol.CredentialStatus_NoSuchTask)
	if rec, _ := h.Tasks.Get(cred); rec.CredentialPubKey != pub {
		t.Fatal("a refused re-key replaced the key")
	}
	// The key the caller would hold proves nothing, so D stays out of its reach.
	stray := credTestConn()
	if st := h.RecordCredentialIdentity(stray.ConnectionID(), stray, taskIDFromHex(cred), newPub); st == protocol.ClientHelloStatus_Ok {
		t.Fatal("the caller's key authenticates as the credential")
	}
	for _, bit := range []protocol.Capability{protocol.Capability_Cancel, protocol.Capability_FileRead} {
		if h.inScope(stray.ConnectionID(), bit, child) {
			t.Fatalf("C's child is in reach for %v through the caller's key", bit)
		}
	}

	// The operator re-keys any credential.
	_, opPub := newCredentialKey(t)
	r, resp = rekey(t, h, op, rekeyRequest(cred, opPub, ttlMs(time.Hour)))
	wantCredStatus(t, "operator re-key", r, resp, protocol.CredentialStatus_Ok)

	// The operator hands C to the caller by reparenting it; then it is the
	// caller's descendant and the re-key is the caller's to make.
	if got := sendSetParent(t, h, op, protocol.SetParentRequest{TaskId: taskIDFromHex(cred), ParentId: taskIDFromHex(gID)}); got.Status != protocol.SetParentStatus_Ok {
		t.Fatalf("set_parent: %v", got.Status)
	}
	r, resp = rekey(t, h, g, rekeyRequest(cred, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "re-key after set_parent under the caller", r, resp, protocol.CredentialStatus_Ok)
	if rec, _ := h.Tasks.Get(cred); rec.CredentialPubKey != newPub {
		t.Fatal("the re-key after set_parent did not replace the key")
	}
}

// A re-key that widens through --caps is still bounded by what the caller
// could issue: it gets the intersection, as an issue would.
func TestCredentialRekeyWideningIsAttenuated(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	global := authority.Scope{Base: protocol.ScopeBase_Global}
	caller, callerID := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | protocol.Capability_ScheduleRun | protocol.Capability_Cancel,
		Scope: global, DelegateCaps: protocol.Capability_All, DelegateScope: global})
	cred := credUnder(t, h, callerID, authority.Grant{Caps: protocol.Capability_ScheduleRun, Scope: global}, pub)
	_, newPub := newCredentialKey(t)
	rk := rekeyRequest(cred, newPub, ttlMs(time.Hour))
	rk.SetCapsPresent(true)
	rk.RequestedCaps = protocol.Capability_ScheduleRun | protocol.Capability_Cancel | protocol.Capability_Spawn
	r, resp := rekey(t, h, caller, rk)
	wantCredStatus(t, "widening re-key", r, resp, protocol.CredentialStatus_Ok)
	if rec, _ := h.Tasks.Get(cred); rec.Capabilities != protocol.Capability_ScheduleRun|protocol.Capability_Cancel {
		t.Fatalf("caps %v, want the caller's share of the request", rec.Capabilities)
	}
}

// Issue-equivalence, ranks: attenuation would clamp the credential's global
// scope to the caller's subtree without an error, so the comparison, not the
// clamp, is what refuses.
func TestCredentialRekeyRefusesWiderRank(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	caller, callerID := credAgent(t, h, authority.Grant{Caps: protocol.Capability_CredentialIssue | protocol.Capability_ScheduleRun,
		Scope:        authority.Scope{Base: protocol.ScopeBase_Subtree},
		DelegateCaps: protocol.Capability_All, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}})
	cred := credUnder(t, h, callerID, authority.Grant{Caps: protocol.Capability_ScheduleRun, Scope: authority.Scope{Base: protocol.ScopeBase_Global}}, pub)
	_, newPub := newCredentialKey(t)
	r, resp := rekey(t, h, caller, rekeyRequest(cred, newPub, ttlMs(time.Hour)))
	wantCredStatus(t, "credential's global scope, caller's subtree", r, resp, protocol.CredentialStatus_ScopeNotPermitted)
	// Narrowing the scope on the re-key makes it issuable.
	rk := rekeyRequest(cred, newPub, ttlMs(time.Hour))
	rk.SetScopePresent(true)
	rk.Scope = authority.Scope{Base: protocol.ScopeBase_None}.ToWire()
	r, resp = rekey(t, h, caller, rk)
	wantCredStatus(t, "narrowed to none", r, resp, protocol.CredentialStatus_Ok)
	if rec, _ := h.Tasks.Get(cred); rec.Scope.Base != protocol.ScopeBase_None {
		t.Fatalf("scope %v after a narrowing re-key", rec.Scope)
	}
}

// --- refusals: a credential is re-keyed, never resumed onto a runner ---

func TestResumePathsRefuseCredential(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	live, err := h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{Caps: protocol.Capability_ScheduleRun}, pub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{Caps: protocol.Capability_ScheduleRun}, pub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	h.Tasks.Cancel(revoked, protocol.EndReason_Cancelled, protocol.TaskID{})
	wantRefusal := func(what, id string, resp protocol.OpenInteractiveResponse) {
		t.Helper()
		msg := string(resp.ErrorMsg)
		if resp.Status != protocol.OpenInteractiveStatus_ResumeNotFound || !strings.Contains(msg, "credential rekey "+id) {
			t.Fatalf("%s: status %v %q, want resume_not_found naming credential rekey", what, resp.Status, msg)
		}
		if rec, _ := h.Tasks.Get(id); rec.Kind != protocol.TaskKind_Credential || rec.Status == protocol.TaskStatus_Queued {
			t.Fatalf("%s: the credential was touched: kind %v status %v", what, rec.Kind, rec.Status)
		}
	}
	for _, id := range []string{live, revoked} {
		fg := foregroundResume(id)
		fg.SetDetached(false)
		resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, fg, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
		wantRefusal("foreground open", id, resp)
		wantRefusal("detached foreground open", id, openBG(h, foregroundResume(id), ""))
		wantRefusal("background open", id, openBG(h, resumeReq(id, bgOpen("claude")), "go"))
		exact := resumeReq(id, bgOpen(""))
		exact.SetResumeExact(true)
		wantRefusal("exact background open", id, openBG(h, exact, ""))
	}

	// The store refuses on its own: a path that reaches Resume without the
	// handler's check must not overwrite the kind and queue it for a runner.
	_, err = h.Tasks.Resume(revoked, "p", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, GrantUpdate{},
		protocol.TaskKind_Interactive, "claude", protocol.TaskID{}, false, RunSpec{Mode: protocol.RunMode_Background})
	if err != ResumeErrCredential {
		t.Fatalf("store Resume of a credential: %v, want ResumeErrCredential", err)
	}
	if rec, _ := h.Tasks.Get(revoked); rec.Kind != protocol.TaskKind_Credential || rec.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("store Resume changed the credential: %v %v", rec.Kind, rec.Status)
	}
}

func TestScheduleBindRefusesCredential(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	_, pub := newCredentialKey(t)
	cred, err := e.h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{Caps: protocol.Capability_ScheduleRun}, pub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	op := schedOpConn()
	r := e.create(t, op.ConnectionID(), bindSpec(t, createSpec(), cred), "p")
	if r.Status != protocol.ScheduleStatus_KindUnsupported || !strings.Contains(string(r.Message), "credential") {
		t.Fatalf("bind of a credential at create: %v %q, want kind_unsupported naming the kind", r.Status, r.Message)
	}
	if len(e.store.List()) != 0 {
		t.Fatal("a refused bind stored a schedule")
	}
	created := e.create(t, op.ConnectionID(), createSpec(), "p")
	if created.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("plain create: %v %s", created.Status, created.Message)
	}
	sp := &protocol.ScheduleSpec{}
	sp.SetTaskPresent(true)
	sp.SetTaskId(taskIDFromHex(cred))
	r = e.edit(t, op.ConnectionID(), schedIDHex(created.Id), sp, "")
	if r.Status != protocol.ScheduleStatus_KindUnsupported || !strings.Contains(string(r.Message), "credential") {
		t.Fatalf("bind of a credential at edit: %v %q, want kind_unsupported naming the kind", r.Status, r.Message)
	}
	if rec, _ := e.h.Tasks.Get(cred); rec.RunMode != protocol.RunMode_Foreground && rec.RunMode != 0 {
		t.Fatalf("a refused bind wrote the credential's run mode: %v", rec.RunMode)
	}
}

// --- set_caps (D12, D14) ---

func TestSetCapsMaskedBitOnCredentialAnswersMaskedCap(t *testing.T) {
	h := newTestHandler(t)
	op := credTestConn()
	_, pub := newCredentialKey(t)
	cred := mustCreateCredential(t, h.Tasks, protocol.TaskID{}, protocol.Capability_Spawn, pub, time.Now().Add(time.Hour))
	// caps naming all is TestSetCapsCannotAddMaskedBitsToCredential; one bit
	// in either half is refused the same way.
	onlyCaps := protocol.SetCapsRequest{TaskId: taskIDFromHex(cred), Caps: protocol.Capability_ScheduleEdit}
	onlyCaps.SetCapsPresent(true)
	onlyDelegate := protocol.SetCapsRequest{TaskId: taskIDFromHex(cred), DelegateCaps: protocol.Capability_CredentialIssue}
	onlyDelegate.SetDelegateCapsPresent(true)
	for name, req := range map[string]protocol.SetCapsRequest{"caps": onlyCaps, "delegate caps": onlyDelegate} {
		if got := sendSetCaps(t, h, op, req); got.Status != protocol.SetCapsStatus_MaskedCap {
			t.Errorf("%s: status %v, want masked_cap", name, got.Status)
		}
	}
	if rec, _ := h.Tasks.Get(cred); rec.Capabilities != protocol.Capability_Spawn {
		t.Fatalf("a refused set_caps changed the credential: %v", rec.Capabilities)
	}
	// Re-grant works unchanged without a masked bit (D12).
	ok := protocol.SetCapsRequest{TaskId: taskIDFromHex(cred), Caps: protocol.Capability_ScheduleRun}
	ok.SetCapsPresent(true)
	if got := sendSetCaps(t, h, op, ok); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("plain re-grant: %v", got.Status)
	}
	// The same bits on an agent task are an ordinary grant.
	agent := plainTask(h)
	all := protocol.SetCapsRequest{TaskId: taskIDFromHex(agent), Caps: protocol.Capability_All}
	all.SetCapsPresent(true)
	if got := sendSetCaps(t, h, op, all); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("set_caps all on an agent task: %v", got.Status)
	}
}

// D14: a credential holding spawn hands neither masked bit to a child.
func TestSpawnFromCredentialCarriesNoMaskedBit(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	cred := mustCreateCredential(t, h.Tasks, protocol.TaskID{}, protocol.Capability_All, pub, time.Now().Add(time.Hour))
	h.Tasks.SetCaps(cred, GrantUpdate{DelegateCaps: true, To: authority.Grant{DelegateCaps: protocol.Capability_All}}, AuthorityWrite{})
	conn := recordCredentialConn(t, h, taskIDFromHex(cred), pub, 9510)
	cid := conn.ConnectionID()
	child := h.granter(cid, h.callerCaps(cid)).ChildCaps(protocol.Capability_All)
	if child&CredentialMaskedCaps != 0 || child&protocol.Capability_Spawn == 0 {
		t.Fatalf("a credential's child would hold %v", child)
	}
}

// Carried item: the control seat reads the principal through principalRecord,
// so a credential connection whose key was replaced holds no seat.
func TestControlSeatOfReplacedCredentialKey(t *testing.T) {
	h := newTestHandler(t)
	_, oldPub := newCredentialKey(t)
	cred, err := h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{Caps: protocol.Capability_Spawn | protocol.Capability_ExecControl,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}}, oldPub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	conn := recordCredentialConn(t, h, taskIDFromHex(cred), oldPub, 9520)
	cid := conn.ConnectionID()
	if !h.controlSeatAllowed(cid, "") {
		t.Fatal("a live credential with a subtree scope has no seat for a new child")
	}
	_, newPub := newCredentialKey(t)
	if _, err := h.Tasks.RekeyCredential(cred, newPub, time.Now().Add(time.Hour), nil, AuthorityWrite{}); err != nil {
		t.Fatal(err)
	}
	if h.controlSeatAllowed(cid, "") {
		t.Fatal("the replaced key's connection still takes a control seat")
	}
}

// --- whoami and list ---

func TestWhoamiReportsCredential(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	cred, err := h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{Caps: protocol.Capability_ScheduleRun}, pub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	whoami := func(conn *fakeConn) *protocol.WhoAmIResponse {
		t.Helper()
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_Whoami, RequestId: 3}}
		req.SetWhoami(protocol.WhoAmIRequest{})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, conn)
		w := resp.Whoami()
		if w == nil {
			t.Fatal("no whoami answer")
		}
		return w
	}
	conn := recordCredentialConn(t, h, taskIDFromHex(cred), pub, 9530)
	rec, _ := h.Tasks.Get(cred)
	w := whoami(conn)
	if w.PrincipalKind != protocol.ClientKind_Credential || w.CredentialExpiresAt != uint64(rec.ExpiresAt.UnixNano()) ||
		hex.EncodeToString(w.PrincipalTaskId.Id[:]) != cred {
		t.Fatalf("credential whoami: kind %v expiry %d principal %x", w.PrincipalKind, w.CredentialExpiresAt, w.PrincipalTaskId.Id)
	}
	agent, _ := credAgent(t, h, authority.Grant{Caps: protocol.Capability_FileRead})
	if w := whoami(agent); w.PrincipalKind != protocol.ClientKind_Agent || w.CredentialExpiresAt != 0 {
		t.Fatalf("agent whoami: kind %v expiry %d", w.PrincipalKind, w.CredentialExpiresAt)
	}
}

func TestListCarriesCredentialFields(t *testing.T) {
	h := newTestHandler(t)
	_, pub := newCredentialKey(t)
	cred, err := h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{Caps: protocol.Capability_ScheduleRun}, pub, time.Now().Add(time.Hour), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	used := time.Now()
	h.Tasks.TouchCredential(cred, used)
	conn := credTestConn()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_List, RequestId: 4}}
	req.SetList(defaultListQuery())
	h.Handle(conn, encodeTaskControlRequest(t, req))
	body := decodeListBody(t, conn.sendStreams[0].bytes)
	rec, _ := h.Tasks.Get(cred)
	for _, ti := range body.Tasks {
		if hex.EncodeToString(ti.Id.Id[:]) != cred {
			continue
		}
		if ti.Kind != protocol.TaskKind_Credential || ti.Status != protocol.TaskStatus_Active ||
			ti.CredentialExpiresAt != uint64(rec.ExpiresAt.UnixNano()) || ti.CredentialLastUsedAt != uint64(used.UnixNano()) ||
			string(ti.Name) != "nightly" {
			t.Fatalf("listed credential: kind %v status %v expiry %d last used %d name %q", ti.Kind, ti.Status,
				ti.CredentialExpiresAt, ti.CredentialLastUsedAt, ti.Name)
		}
		return
	}
	t.Fatal("the credential is not in the list")
}

// --- end to end over the in-process server ---

// An operator issues {schedule_run, resume_exact} scoped to T; a script holding
// only the key fires the schedule bound to T. A manual run is recorded as
// run_now (S-D13); the credential's own resume of T is attributed to it in the
// schedule's history as resumed_by with kind credential (D6).
func TestCredentialFiresBoundScheduleEndToEnd(t *testing.T) {
	st, err := LoadScheduleStore("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, e := startPubsubAuthzServer(t, func(c *Config) { c.Schedules = st })
	target, err := e.s.Tasks().CreateWithinLimits("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()}, "claude",
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Tasks().Cancel(target, protocol.EndReason_Cancelled, protocol.TaskID{})
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, target)
	sc.NextAt = time.Now().Add(time.Hour)
	if err := st.Put(sc); err != nil {
		t.Fatal(err)
	}

	op := e.dialOperator(ctx, t)
	priv, pub := newCredentialKey(t)
	ir := issueRequest(pub, ttlMs(time.Hour), protocol.Capability_ScheduleRun|protocol.Capability_ResumeExact,
		authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{target}})
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialIssue}}
	req.SetCredentialIssue(ir)
	resp, err := op.RoundTripTaskControl(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	cr := resp.CredentialIssue()
	if cr == nil || cr.Status != protocol.CredentialStatus_Ok {
		t.Fatalf("operator issue answered %v %+v", resp.Kind, cr)
	}

	c := e.dialCredential(ctx, t, cr.TaskId, priv)
	if c.status != protocol.PskAuthStatus_Ok {
		t.Fatalf("handshake with the issued key: %v", c.status)
	}
	run := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleRun}}
	run.SetScheduleRun(protocol.ScheduleIDRequest{Id: scheduleIDFromHex(sc.ID)})
	resp, err = c.RoundTripTaskControl(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.ScheduleRun(); r == nil || r.Status != protocol.ScheduleStatus_Ok || !strings.HasPrefix(string(r.Message), "run_now") {
		t.Fatalf("schedule run from the credential answered %v %+v", resp.Kind, resp.ScheduleRun())
	}
	if rec, _ := e.s.Tasks().Get(target); rec.Status != protocol.TaskStatus_Queued || rec.Run != 2 {
		t.Fatalf("the bound task after the run: status %v run %d", rec.Status, rec.Run)
	}

	// The credential resumes T itself, exactly (resume_exact is what it holds).
	e.s.Tasks().Cancel(target, protocol.EndReason_Cancelled, protocol.TaskID{})
	oi := resumeReq(target, bgOpen(""))
	oi.SetResumeExact(true)
	open := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive}}
	open.SetOpenInteractive(*oi)
	resp, err = c.RoundTripTaskControl(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.OpenInteractive(); r == nil || r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("credential resume_exact of the bound task answered %v %+v", resp.Kind, resp.OpenInteractive())
	}
	if rec, _ := e.s.Tasks().Get(target); rec.ResumedByKind != protocol.ClientKind_Credential {
		t.Fatalf("resumed_by_kind %v, want credential", rec.ResumedByKind)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, _ := st.Get(sc.ID)
		var kinds []string
		for _, h := range got.History {
			kinds = append(kinds, scheduleOutcomeName(h.Outcome)+"/"+h.ByKind.String())
			if h.Outcome == protocol.ScheduleOutcome_ResumedBy && h.ByKind == protocol.ClientKind_Credential {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("history %v has no resumed_by attributed to a credential", kinds)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// M1: a name reaches operators' terminals, so one over 64 bytes, not UTF-8 or
// holding a control character is refused at issue, with nothing created.
func TestCredentialIssueRefusesBadName(t *testing.T) {
	h := newTestHandler(t)
	op := credTestConn()
	_, pub := newCredentialKey(t)
	for name, n := range map[string]string{
		"65 bytes":  strings.Repeat("a", 65),
		"escape":    "ci\x1b]0;pwned\x07",
		"newline":   "ci\nforged row",
		"del":       "ci\x7f",
		"c1 csi":    "ci\u009b2J",
		"not utf-8": "ci\xff",
	} {
		r := issueRequest(pub, ttlMs(time.Hour), 0, authority.Scope{})
		r.SetName([]byte(n))
		got, resp := issue(t, h, op, r)
		wantCredStatus(t, name, got, resp, protocol.CredentialStatus_BadName)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("refused issues created %d tasks", n)
	}
	for name, n := range map[string]string{"64 bytes": strings.Repeat("a", 64), "non-ASCII text": "夜間ビルド", "empty": ""} {
		r := issueRequest(pub, ttlMs(time.Hour), 0, authority.Scope{})
		r.SetName([]byte(n))
		got, resp := issue(t, h, op, r)
		wantCredStatus(t, name, got, resp, protocol.CredentialStatus_Ok)
	}
}
