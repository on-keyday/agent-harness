package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
)

// newCredentialKey is a fresh Ed25519 pair, the public half as the store
// keeps it.
func newCredentialKey(t *testing.T) (ed25519.PrivateKey, [32]byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var k [32]byte
	copy(k[:], pub)
	return priv, k
}

func buildCredentialAuthReq(tid protocol.TaskID, priv ed25519.PrivateKey, transcript []byte) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Credential}
	req.SetBinder(nil)
	auth := protocol.CredentialAuth{TaskId: tid}
	copy(auth.Sig[:], cli.SignCredentialAuth(priv, transcript))
	req.SetCredentialAuth(auth)
	return req
}

// issueCredential creates a credential task in s's store, issued by the
// operator, holding g until expiresAt.
func issueCredential(t *testing.T, s *TaskStore, g authority.Grant, pub [32]byte, expiresAt time.Time) (string, protocol.TaskID) {
	t.Helper()
	id, err := s.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, g, pub, expiresAt, "ci")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	return id, taskIDFromHex(id)
}

// credConn is a connection that authenticated (or tried to) as a credential.
// It speaks the wire directly: the client side of the credential handshake is
// a later change, and this test is about what the SERVER accepts.
type credConn struct {
	pc     *peer.Conn
	status protocol.PskAuthStatus
	mu     sync.Mutex
	next   uint32
	waits  map[uint32]chan *protocol.TaskControlResponse
}

// dialCredential runs the credential handshake against e's server and
// returns the connection with the status the server answered.
func (e *pubsubAuthzServer) dialCredential(ctx context.Context, t *testing.T, tid protocol.TaskID, priv ed25519.PrivateKey) *credConn {
	t.Helper()
	pc, err := cli.DialPeerConn(ctx, e.cid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	c := &credConn{pc: pc, waits: map[uint32]chan *protocol.TaskControlResponse{}}
	authed := make(chan protocol.PskAuthStatus, 1)
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		switch kind {
		case appwire.AppKind_PskAuth:
			var resp protocol.PskAuthResponse
			if err := resp.DecodeExact(payload); err == nil {
				select {
				case authed <- resp.Status:
				default:
				}
			}
		case appwire.AppKind_TaskControl:
			resp := &protocol.TaskControlResponse{}
			if _, err := resp.Decode(payload); err != nil {
				return
			}
			c.mu.Lock()
			ch := c.waits[resp.RequestId]
			delete(c.waits, resp.RequestId)
			c.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	})
	pc.Start(ctx)
	req := buildCredentialAuthReq(tid, priv, pc.Connection().GetTranscript())
	if _, _, err := pc.Connection().SendMessage(buildPskAuthData(t, req)); err != nil {
		t.Fatal(err)
	}
	select {
	case c.status = <-authed:
	case <-ctx.Done():
		t.Fatal("credential handshake timed out")
	}
	return c
}

// RoundTripTaskControl makes credConn a cli task-control client, so the cli
// helpers (WhoAmIWith, …) run over it unchanged.
func (c *credConn) RoundTripTaskControl(ctx context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	ch := make(chan *protocol.TaskControlResponse, 1)
	c.mu.Lock()
	id := c.next
	c.next++
	c.waits[id] = ch
	c.mu.Unlock()
	req.Header.RequestId = id
	if _, _, err := c.pc.Connection().SendMessage(req.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	select {
	case r := <-ch:
		return r, nil
	case <-c.pc.Done():
		return nil, fmt.Errorf("connection closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// serverCIDOf is the server-side id of the connection whose principal is tid,
// once its identity is recorded.
func (e *pubsubAuthzServer) serverCIDOf(ctx context.Context, t *testing.T, tid protocol.TaskID) string {
	t.Helper()
	for {
		e.s.activeConnsMu.Lock()
		var found string
		for cid := range e.s.activeConns {
			if e.s.taskHandler.lookupPrincipal(cid.String()) == tid {
				found = cid.String()
			}
		}
		e.s.activeConnsMu.Unlock()
		if found != "" {
			return found
		}
		if ctx.Err() != nil {
			t.Fatal("no connection was recorded with the credential as its principal")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantClosed(t *testing.T, c *credConn, within time.Duration, why string) {
	t.Helper()
	select {
	case <-c.pc.Done():
	case <-time.After(within):
		t.Fatalf("connection still open %v after %s", within, why)
	}
}

// --- the handshake (D4, D5) ---

func TestCredentialHandshakeAccepts(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	priv, pub := newCredentialKey(t)
	id, tid := issueCredential(t, e.s.Tasks(), authority.Grant{Caps: protocol.Capability_ScheduleRun}, pub, time.Now().Add(time.Hour))
	c := e.dialCredential(ctx, t, tid, priv)
	if c.status != protocol.PskAuthStatus_Ok {
		t.Fatalf("status %v, want ok", c.status)
	}
	w, err := cli.WhoAmIWith(ctx, c)
	if err != nil {
		t.Fatalf("whoami over the credential connection: %v", err)
	}
	if w.PrincipalTaskId != tid {
		t.Fatalf("whoami principal %x, want the credential %s", w.PrincipalTaskId.Id, id)
	}
	cid := e.serverCIDOf(ctx, t, tid)
	if k := e.s.taskHandler.lookupClientKind(cid); k != protocol.ClientKind_Credential {
		t.Errorf("client kind %v, want credential", k)
	}
	if rec, _ := e.s.Tasks().Get(id); rec.LastUsedAt.IsZero() {
		t.Error("an accepted handshake did not record LastUsedAt")
	}
	// D6, server.go connInfoFor: a credential conn shows its principal.
	var listed bool
	for _, ci := range e.s.ConnList(protocol.TaskID{}, true) {
		if string(ci.Cid) == cid {
			listed = true
			if ci.Role != protocol.ConnRole_Credential || ci.PrincipalTask != tid {
				t.Errorf("conn row role=%v principal=%x, want credential/%s", ci.Role, ci.PrincipalTask.Id, id)
			}
		}
	}
	if !listed {
		t.Error("the credential connection is not in the conn listing")
	}
}

func refusedWith(t *testing.T, want protocol.PskAuthStatus, mk func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey)) {
	t.Helper()
	ctx, e := startPubsubAuthzServer(t)
	tid, priv := mk(e)
	c := e.dialCredential(ctx, t, tid, priv)
	if c.status != want {
		t.Fatalf("status %v, want %v", c.status, want)
	}
	wantClosed(t, c, 3*time.Second, "a refused handshake")
}

func TestCredentialHandshakeWrongKeyIsBadTicket(t *testing.T) {
	refusedWith(t, protocol.PskAuthStatus_BadTicket, func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey) {
		_, pub := newCredentialKey(t)
		other, _ := newCredentialKey(t)
		_, tid := issueCredential(t, e.s.Tasks(), authority.Grant{}, pub, time.Now().Add(time.Hour))
		return tid, other
	})
}

func TestCredentialHandshakeUnknownTaskIsBadTicket(t *testing.T) {
	refusedWith(t, protocol.PskAuthStatus_BadTicket, func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey) {
		priv, _ := newCredentialKey(t)
		var tid protocol.TaskID
		_, _ = rand.Read(tid.Id[:])
		return tid, priv
	})
}

// The anti-goal's first door: the zero id names no task, and must not name the
// operator either.
func TestCredentialHandshakeZeroTaskIsBadTicket(t *testing.T) {
	refusedWith(t, protocol.PskAuthStatus_BadTicket, func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey) {
		priv, _ := newCredentialKey(t)
		return protocol.TaskID{}, priv
	})
}

func TestCredentialHandshakeNonCredentialTaskIsBadTicket(t *testing.T) {
	refusedWith(t, protocol.PskAuthStatus_BadTicket, func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey) {
		priv, _ := newCredentialKey(t)
		return taskIDFromHex(e.victim), priv
	})
}

func TestCredentialHandshakeExpired(t *testing.T) {
	refusedWith(t, protocol.PskAuthStatus_Expired, func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey) {
		priv, pub := newCredentialKey(t)
		_, tid := issueCredential(t, e.s.Tasks(), authority.Grant{}, pub, time.Now().Add(-time.Second))
		return tid, priv
	})
}

func TestCredentialHandshakeRevoked(t *testing.T) {
	refusedWith(t, protocol.PskAuthStatus_Revoked, func(e *pubsubAuthzServer) (protocol.TaskID, ed25519.PrivateKey) {
		priv, pub := newCredentialKey(t)
		id, tid := issueCredential(t, e.s.Tasks(), authority.Grant{}, pub, time.Now().Add(time.Hour))
		e.s.Tasks().Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
		return tid, priv
	})
}

// D5: expired and revoked are told only to the key holder. Without the key,
// an ended credential answers exactly as an unknown task does.
func TestCredentialGateRevealsStateOnlyToKeyHolder(t *testing.T) {
	s := NewTaskStore()
	_, pub := newCredentialKey(t)
	stranger, _ := newCredentialKey(t)
	expiredID, expired := issueCredential(t, s, authority.Grant{}, pub, time.Now().Add(-time.Second))
	revokedID, revoked := issueCredential(t, s, authority.Grant{}, pub, time.Now().Add(time.Hour))
	s.Cancel(revokedID, protocol.EndReason_Cancelled, protocol.TaskID{})
	_ = expiredID
	for name, tid := range map[string]protocol.TaskID{"expired": expired, "revoked": revoked} {
		g := newPSKGate()
		g.CredentialFor = func(tid protocol.TaskID, now time.Time) ([32]byte, CredentialState, bool) {
			return s.CredentialLive(hex.EncodeToString(tid.Id[:]), now)
		}
		var sent []byte
		_, shouldClose, accepted := g.Check(buildPskAuthData(t, buildCredentialAuthReq(tid, stranger, testTranscript)), testTranscript, func(b []byte) { sent = append(sent, b...) })
		if !shouldClose || accepted != nil || g.Authed() {
			t.Fatalf("%s: a stranger's signature was accepted", name)
		}
		if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_BadTicket {
			t.Errorf("%s credential, wrong key: status %v, want bad_ticket", name, st)
		}
	}
}

// A gate with no credential lookup wired refuses the role rather than
// accepting it with nothing checked.
func TestCredentialGateUnwiredRefuses(t *testing.T) {
	priv, _ := newCredentialKey(t)
	var tid protocol.TaskID
	tid.Id[0] = 1
	g := newPSKGate()
	var sent []byte
	_, shouldClose, accepted := g.Check(buildPskAuthData(t, buildCredentialAuthReq(tid, priv, testTranscript)), testTranscript, func(b []byte) { sent = append(sent, b...) })
	if !shouldClose || accepted != nil || g.Authed() {
		t.Fatal("an unwired gate accepted a credential")
	}
	if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_BadTicket {
		t.Errorf("status %v, want bad_ticket", st)
	}
}

func TestCredentialRoleAllowsMessage(t *testing.T) {
	g := newPSKGate()
	g.authed, g.role = true, protocol.AuthRole_Credential
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"task control", []byte{byte(appwire.AppKind_TaskControl), byte(protocol.TaskControlKind_Whoami)}, true},
		{"a second client hello", []byte{byte(appwire.AppKind_TaskControl), byte(protocol.TaskControlKind_ClientHello)}, false},
		{"pubsub", []byte{byte(appwire.AppKind_Pubsub), 0}, true},
		{"telemetry", []byte{byte(appwire.AppKind_Telemetry), 0}, true},
		{"runner control", []byte{byte(appwire.AppKind_RunnerControl), byte(protocol.RunnerMessageType_TaskFinished)}, false},
	}
	for _, c := range cases {
		if got := g.AllowsMessage(c.data); got != c.want {
			t.Errorf("%s: AllowsMessage = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- the principal (anti-goal 1) ---

func TestCredentialPrincipalIsNeverZero(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	priv, pub := newCredentialKey(t)
	// Asked for everything; the store masks credential_issue and schedule_edit.
	id, tid := issueCredential(t, e.s.Tasks(), authority.Grant{Caps: protocol.Capability_All}, pub, time.Now().Add(time.Hour))
	c := e.dialCredential(ctx, t, tid, priv)
	if c.status != protocol.PskAuthStatus_Ok {
		t.Fatalf("handshake: %v", c.status)
	}
	cid := e.serverCIDOf(ctx, t, tid)
	if p := e.s.taskHandler.lookupPrincipal(cid); p != tid {
		t.Fatalf("principal %x, want %s", p.Id, id)
	}
	if caps := e.s.taskHandler.callerCaps(cid); caps == protocol.Capability_All {
		t.Fatal("a credential connection resolved to the operator's capability set")
	}

	sc := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleCreate}}
	sc.SetScheduleCreate(protocol.ScheduleCreateRequest{})
	resp, err := c.RoundTripTaskControl(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if pd := resp.PermissionDenied(); resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil || pd.RequiredCap != protocol.Capability_ScheduleEdit {
		t.Fatalf("schedule create answered %v, want permission_denied for schedule_edit", resp.Kind)
	}

	// Operator-only verbs read "no principal" as the operator; a credential
	// has one.
	setCaps := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SetCaps}}
	setCaps.SetSetCaps(protocol.SetCapsRequest{TaskId: tid})
	resp, err = c.RoundTripTaskControl(ctx, setCaps)
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.SetCaps(); r == nil || r.Status != protocol.SetCapsStatus_NotOperator {
		t.Fatalf("set_caps from a credential answered %+v, want not_operator", resp.SetCaps())
	}
}

// Every path that records a credential principal refuses the zero id.
func TestRecordCredentialIdentityRefusesZero(t *testing.T) {
	h := newTestTaskHandler(NewTaskStore())
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9300-1")}
	if st := h.RecordCredentialIdentity(conn.id.String(), conn, protocol.TaskID{}); st == protocol.ClientHelloStatus_Ok {
		t.Fatal("a zero credential principal was recorded")
	}
	if k := h.lookupClientKind(conn.id.String()); k == protocol.ClientKind_Credential {
		t.Fatal("a refused credential left its kind recorded")
	}
	// Through the gate's dispatch, too.
	d := &Dispatcher{RecordCredentialIdentity: h.RecordCredentialIdentity}
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Credential}
	req.SetCredentialAuth(protocol.CredentialAuth{})
	if pskDispatchIdentity(d, conn, req) {
		t.Fatal("pskDispatchIdentity accepted a zero credential principal")
	}
	// And an unwired dispatcher closes the connection rather than leaving it
	// authenticated with no principal, which reads as the operator.
	priv, pub := newCredentialKey(t)
	_ = priv
	_, tid := issueCredential(t, h.Tasks, authority.Grant{}, pub, time.Now().Add(time.Hour))
	req.SetCredentialAuth(protocol.CredentialAuth{TaskId: tid})
	if pskDispatchIdentity(&Dispatcher{}, conn, req) {
		t.Fatal("pskDispatchIdentity accepted a credential with nothing to record it")
	}
}

func TestClientHelloClaimingCredentialKindRefused(t *testing.T) {
	h := newTestTaskHandler(NewTaskStore())
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9301-1")}
	hello := &protocol.ClientHello{Kind: protocol.ClientKind_Credential}
	if st := h.RecordClientIdentity(conn.id.String(), conn, hello); st != protocol.ClientHelloStatus_NotPermitted {
		t.Fatalf("a client hello claiming credential: %v, want not_permitted", st)
	}
	if k := h.lookupClientKind(conn.id.String()); k != protocol.ClientKind_Unspecified {
		t.Fatalf("kind %v recorded for a refused hello", k)
	}
}

// --- liveness (D8, D9, Review Focus 1) ---

// recordCredentialConn records a live credential's connection on h, as the
// gate's dispatch would.
func recordCredentialConn(t *testing.T, h *TaskHandler, tid protocol.TaskID, port int) *fakeConn {
	t.Helper()
	conn := &fakeConn{id: objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%d-1", port))}
	if st := h.RecordCredentialIdentity(conn.id.String(), conn, tid); st != protocol.ClientHelloStatus_Ok {
		t.Fatalf("RecordCredentialIdentity: %v", st)
	}
	return conn
}

func wantSeesNothing(t *testing.T, h *TaskHandler, cid string) {
	t.Helper()
	if caps := h.callerCaps(cid); caps != protocol.Capability_None {
		t.Errorf("callerCaps = %v, want none", caps)
	}
	all, allowed := h.visibleToCaller(cid)
	if all || len(allowed) != 0 {
		t.Errorf("visibleToCaller = all:%v %v, want nothing", all, allowed)
	}
	all, allowed, parent := h.listVisibleToCaller(cid)
	if all || len(allowed) != 0 || parent != "" {
		t.Errorf("listVisibleToCaller = all:%v %v parent:%q, want nothing", all, allowed, parent)
	}
	all, allowed = h.scopeSet(cid, protocol.Capability_Cancel)
	if all || len(allowed) != 0 {
		t.Errorf("scopeSet(cancel) = all:%v %v, want nothing", all, allowed)
	}
}

// D8: a connection opened before expiry is refused its first request after
// it, with no deadline watcher running to end the task.
func TestCredentialDeadBeforeTimerSeesNothing(t *testing.T) {
	tasks := NewTaskStore()
	h := newTestTaskHandler(tasks)
	parent := tasks.Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	target := tasks.Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	_, pub := newCredentialKey(t)
	id, err := tasks.CreateCredential(taskIDFromHex(parent), protocol.ClientKind_Cli, authority.Grant{
		Caps:  protocol.Capability_Cancel,
		Scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{target}},
	}, pub, time.Now().Add(time.Hour), "ci")
	if err != nil {
		t.Fatal(err)
	}
	conn := recordCredentialConn(t, h, taskIDFromHex(id), 9310)
	cid := conn.id.String()
	if !h.authorize(cid, protocol.Capability_Cancel, target) {
		t.Fatal("a live credential cannot act on its scoped target")
	}
	if _, allowed, p := h.listVisibleToCaller(cid); !allowed[target] || p != parent {
		t.Fatalf("live credential: visible %v parent %q", allowed, p)
	}

	// The expiry passes; nothing has ended the task.
	tasks.mu.Lock()
	tasks.tasks[id].ExpiresAt = time.Now().Add(-time.Millisecond)
	tasks.mu.Unlock()
	if rec, _ := tasks.Get(id); rec.Status != protocol.TaskStatus_Active {
		t.Fatalf("status %v, want the row still active (no watcher)", rec.Status)
	}

	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_Cancel, RequestId: 7}}
	req.SetCancel(protocol.CancelTask{TaskId: taskIDFromHex(target)})
	h.Handle(conn, encodeTaskControlRequest(t, req))
	if resp := conn.lastTaskControlResponse(t); resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("first request after expiry answered %v, want permission_denied", resp.Kind)
	}
	if rec, _ := tasks.Get(target); rec.Status == protocol.TaskStatus_Cancelled {
		t.Fatal("an expired credential cancelled its target")
	}
	wantSeesNothing(t, h, cid)
}

func TestRevokedCredentialSeesNothing(t *testing.T) {
	tasks := NewTaskStore()
	h := newTestTaskHandler(tasks)
	_, pub := newCredentialKey(t)
	id, _ := issueCredential(t, tasks, authority.Grant{Caps: protocol.Capability_Cancel, Scope: authority.Scope{Base: protocol.ScopeBase_Global}}, pub, time.Now().Add(time.Hour))
	conn := recordCredentialConn(t, h, taskIDFromHex(id), 9311)
	tasks.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	wantSeesNothing(t, h, conn.id.String())
}

// Review Focus 1: a principal whose record is gone (a credential pruned after
// its revoke while a connection stayed open) holds nothing and sees nothing —
// not the default subtree scope a missing record used to fall back to.
func TestMissingPrincipalRecordSeesNothing(t *testing.T) {
	tasks := NewTaskStore()
	h := newTestTaskHandler(tasks)
	_, pub := newCredentialKey(t)
	id, tid := issueCredential(t, tasks, authority.Grant{Caps: protocol.Capability_Spawn}, pub, time.Now().Add(time.Hour))
	conn := recordCredentialConn(t, h, tid, 9312)
	// A child it spawned outlives it.
	tasks.Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Credential, tid, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	tasks.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	if removed, _, _ := tasks.PruneByIDs(nil, []string{id}); removed != 1 {
		t.Fatalf("prune removed %d", removed)
	}
	wantSeesNothing(t, h, conn.id.String())

	// The same for an agent principal: the fallback was not credential-specific.
	agentConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9313-1")}
	var gone protocol.TaskID
	gone.Id[0] = 0x42
	hello := &protocol.ClientHello{Kind: protocol.ClientKind_Agent}
	hello.SetAgentInfo(protocol.AgentInfo{TaskId: gone, RunnerId: testRunnerID("gone")})
	if st := h.RecordClientIdentity(agentConn.id.String(), agentConn, hello); st != protocol.ClientHelloStatus_Ok {
		t.Fatalf("agent hello: %v", st)
	}
	tasks.Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, gone, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	wantSeesNothing(t, h, agentConn.id.String())
}

// D9: revoke closes a live credential connection, and closes it with a wire
// close — a local close alone leaves the peer to find out from its GC.
func TestRevokeClosesLiveCredentialConn(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	priv, pub := newCredentialKey(t)
	id, tid := issueCredential(t, e.s.Tasks(), authority.Grant{}, pub, time.Now().Add(time.Hour))
	c := e.dialCredential(ctx, t, tid, priv)
	if c.status != protocol.PskAuthStatus_Ok {
		t.Fatalf("handshake: %v", c.status)
	}
	e.serverCIDOf(ctx, t, tid)
	e.s.Tasks().Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	wantClosed(t, c, 3*time.Second, "the credential was revoked")
}

// Expiry ends the task through the same cancel path, so it closes the
// connection too (D9).
func TestExpiryClosesLiveCredentialConn(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	priv, pub := newCredentialKey(t)
	id, tid := issueCredential(t, e.s.Tasks(), authority.Grant{}, pub, time.Now().Add(time.Hour))
	c := e.dialCredential(ctx, t, tid, priv)
	if c.status != protocol.PskAuthStatus_Ok {
		t.Fatalf("handshake: %v", c.status)
	}
	e.serverCIDOf(ctx, t, tid)
	if !e.s.Tasks().ExpireCredential(id, time.Now().Add(2*time.Hour)) {
		t.Fatal("ExpireCredential ended nothing")
	}
	wantClosed(t, c, 3*time.Second, "the credential expired")
}

// set_caps' caller of the same helper gets the wire close as well: a narrowed
// agent's client learns at once that it must reconnect.
func TestDropConnsForPrincipalSendsWireClose(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	agent, id := e.dialAgentTask(ctx, t, "narrowed", authority.Grant{Caps: protocol.Capability_ExecView})
	if n := e.s.dropConnsForPrincipal(id); n != 1 {
		t.Fatalf("dropConnsForPrincipal closed %d, want 1", n)
	}
	select {
	case <-agent.Peer().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the agent's connection is still open: no wire close reached it")
	}
}

// --- pubsub (D10) ---

// readUntil reads frames from st until one contains want, failing if a frame
// containing never arrives first.
func readUntil(ctx context.Context, t *testing.T, st pubsub.StreamReader, want, never []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	type result struct {
		leaked bool
		err    error
	}
	got := make(chan result, 1)
	go func() {
		fr := pubsub.NewFrameReader(st)
		for {
			p, err := fr.Next()
			if err != nil {
				got <- result{err: err}
				return
			}
			if bytes.Contains(p, never) {
				got <- result{leaked: true}
				return
			}
			if bytes.Contains(p, want) {
				got <- result{}
				return
			}
		}
	}()
	select {
	case r := <-got:
		if r.leaked {
			t.Fatal("an event of a task outside the credential's visibility reached it")
		}
		if r.err != nil {
			t.Fatalf("stream ended before the visible task's event: %v", r.err)
		}
	case <-ctx.Done():
		t.Fatal("the visible task's event never arrived")
	}
}

func TestCredentialPubsubSeesOnlyVisibleTaskEvents(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	visible := e.s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	hidden := e.victim
	priv, pub := newCredentialKey(t)
	id, tid := issueCredential(t, e.s.Tasks(), authority.Grant{
		Caps:  protocol.Capability_ScheduleRun,
		Scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{visible}},
	}, pub, time.Now().Add(time.Hour))
	c := e.dialCredential(ctx, t, tid, priv)
	if c.status != protocol.PskAuthStatus_Ok {
		t.Fatalf("handshake: %v", c.status)
	}
	e.serverCIDOf(ctx, t, tid)

	_, err := c.pc.JoinAndGetStream(ctx, "cred", topics.Notifications())
	wantForbidden(t, "a scoped credential", topics.Notifications(), err)

	st, err := c.pc.JoinAndGetStream(ctx, "cred", topics.TasksStatus())
	if err != nil {
		t.Fatalf("credential JOIN %s: %v", topics.TasksStatus(), err)
	}
	// The hidden task's event is published first: had it been delivered, it
	// would arrive before the visible one's.
	e.s.Tasks().Cancel(hidden, protocol.EndReason_Cancelled, protocol.TaskID{})
	e.s.Tasks().Cancel(visible, protocol.EndReason_Cancelled, protocol.TaskID{})
	hiddenID, visibleID := taskIDFromHex(hidden), taskIDFromHex(visible)
	readUntil(ctx, t, st, visibleID.Id[:], hiddenID.Id[:])

	e.s.Tasks().Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	wantClosed(t, c, 3*time.Second, "the credential was revoked")
}
