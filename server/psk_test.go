package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// testTranscript is a stand-in for a connection's handshake transcript.
var testTranscript = []byte("handshake-transcript-bytes")

// pskBinder computes the expected HMAC binder using cli.ComputePSKBinder.
// The call is BYTE-IDENTICAL to what pskGate.Check uses internally:
//
//	cli.ComputePSKBinder(g.psk, transcript)
//
// No inputs were changed; this helper locks in the MITM-resistance test.
func pskBinder(t *testing.T, psk, transcript []byte) []byte {
	t.Helper()
	b, err := cli.ComputePSKBinder(psk, transcript)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildPskAuthData encodes a PskAuthRequest and prepends the AppKind_PskAuth byte.
func buildPskAuthData(t *testing.T, req *protocol.PskAuthRequest) []byte {
	t.Helper()
	data, err := req.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		t.Fatalf("buildPskAuthData: encode failed: %v", err)
	}
	return data
}

// buildOperatorClientHello builds a PskAuthRequest for an operator (kind=Cli, no AgentInfo).
func buildOperatorClientHello(psk, transcript []byte) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
	if len(psk) > 0 {
		binder, _ := cli.ComputePSKBinder(psk, transcript)
		req.SetBinder(binder)
	}
	req.SetClientHello(protocol.ClientHello{Kind: protocol.ClientKind_Cli})
	return req
}

// buildRunnerDispatchReq is a role=runner request for tests that only dispatch
// it (pskDispatchIdentity), which reads the hello and not the proof.
func buildRunnerDispatchReq(rh protocol.RunnerHello) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Runner}
	req.SetRunnerAuth(protocol.RunnerAuth{Hello: rh})
	return req
}

// parsePskResponse extracts the PskAuthStatus from a response.
func parsePskResponse(t *testing.T, sent []byte) protocol.PskAuthStatus {
	t.Helper()
	if len(sent) < 2 {
		t.Fatalf("parsePskResponse: too short: %v", sent)
	}
	if appwire.AppKind(sent[0]) != appwire.AppKind_PskAuth {
		t.Fatalf("parsePskResponse: expected AppKind_PskAuth, got %v", sent[0])
	}
	var resp protocol.PskAuthResponse
	if err := resp.DecodeExact(sent[1:]); err != nil {
		t.Fatalf("parsePskResponse: decode: %v", err)
	}
	return resp.Status
}

// ------- Gate unit tests -------

func TestPSKGate_ValidBinderOperatorClient(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate()
	g.operatorPSK = psk
	if g.Authed() {
		t.Fatal("gate must not be pre-authenticated")
	}

	req := buildOperatorClientHello(psk, testTranscript)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK {
		t.Error("isPSK: want true")
	}
	if shouldClose {
		t.Error("shouldClose: want false")
	}
	if !g.Authed() {
		t.Error("gate must be authed after valid binder + identity")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil on success")
	}
	if accepted.ClientHello() == nil {
		t.Error("accepted.ClientHello(): want non-nil")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

// Operator-secret model: operator-surface connections (kind=cli/tui/webui)
// must prove operatorPSK via the binder. An in-task agent does not hold it, so
// a binder it computes over any other secret cannot authenticate as operator —
// this closes the operator-kind escalation.

func TestPSKGate_OperatorClientRejectsAnotherSecret(t *testing.T) {
	otherSecret := []byte("some-other-secret") // anything an agent could hold
	operatorPSK := []byte("operator-secret")   // operator-only, never given to agents
	g := newPSKGate()
	g.operatorPSK = operatorPSK

	// Simulate an in-task agent claiming kind=Cli to be treated as operator.
	req := buildOperatorClientHello(otherSecret, testTranscript)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || !shouldClose {
		t.Errorf("operator client with another secret: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() {
		t.Error("gate must NOT auth an operator client that did not prove the operator psk")
	}
	if accepted != nil {
		t.Error("accepted: want nil (escalation must be rejected)")
	}
	if status := parsePskResponse(t, sent); status != protocol.PskAuthStatus_BadPsk {
		t.Errorf("response status: got %v, want BadPsk", status)
	}
}

func TestPSKGate_OperatorClientAcceptsOperatorPSK(t *testing.T) {
	operatorPSK := []byte("operator-secret")
	g := newPSKGate()
	g.operatorPSK = operatorPSK

	// A real operator computes its binder with the operator secret.
	req := buildOperatorClientHello(operatorPSK, testTranscript)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("operator client with operator psk: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must auth an operator client that proved the operator psk")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil")
	}
	if status := parsePskResponse(t, sent); status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

// An agent proves its ticket; the operator PSK is not asked of it even when one
// is configured.
func TestPSKGate_AgentProvesItsTicketNotTheOperatorPSK(t *testing.T) {
	rid, tid, ticket := testRunnerID("r"), protocol.TaskID{Id: [16]byte{0xAA, 0xBB}}, [16]byte{1, 2, 3}
	g := agentGate(rid, tid, ticket)
	g.operatorPSK = []byte("operator-secret")

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(buildPskAuthData(t, buildAgentBinderReq(ticket, testTranscript, rid, tid)), testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("agent: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() || accepted == nil {
		t.Errorf("an agent proving its ticket must be accepted when operatorPSK is set: %v", parsePskResponse(t, sent))
	}
}

// A binder keyed by a ticket other than the task's is bad_ticket, and the
// agent does not fall through to operator.
func TestPSKGate_AgentWrongTicket(t *testing.T) {
	rid, tid := testRunnerID("r"), protocol.TaskID{Id: [16]byte{0xCC, 0xDD}}
	g := agentGate(rid, tid, [16]byte{1})
	g.operatorPSK = []byte("s3cr3t")

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(buildPskAuthData(t, buildAgentBinderReq([16]byte{0xFF}, testTranscript, rid, tid)), testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || !shouldClose {
		t.Errorf("bad ticket: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() || accepted != nil {
		t.Error("gate must NOT accept an agent with the wrong ticket")
	}
	if status := parsePskResponse(t, sent); status != protocol.PskAuthStatus_BadTicket {
		t.Errorf("response status: got %v, want BadTicket", status)
	}
}

func TestPSKGate_BadBinder(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate()
	g.operatorPSK = psk

	// Build request with binder from wrong PSK.
	req := buildOperatorClientHello([]byte("wrong"), testTranscript)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || !shouldClose {
		t.Errorf("bad binder: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() {
		t.Error("gate must NOT be authed after bad binder")
	}
	if accepted != nil {
		t.Error("accepted: want nil on bad binder")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_BadPsk {
		t.Errorf("response status: got %v, want BadPsk", status)
	}
}

func TestPSKGate_TranscriptMismatch(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate()
	g.operatorPSK = psk

	// Binder computed with the correct PSK but over a different transcript
	// (simulates what an MITM relaying the client leg would present).
	req := buildOperatorClientHello(psk, []byte("other-leg-transcript"))
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || !shouldClose {
		t.Errorf("transcript mismatch: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() {
		t.Error("gate must NOT auth a binder bound to a different transcript")
	}
	if accepted != nil {
		t.Error("accepted: want nil on transcript mismatch")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_BadPsk {
		t.Errorf("response status: got %v, want BadPsk", status)
	}
}

func TestPSKGate_NoPSKValidIdentity(t *testing.T) {
	// No PSK configured: binder_len=0, but identity is still required.
	g := newPSKGate()
	if g.Authed() {
		t.Fatal("gate with nil PSK must NOT be pre-authenticated (identity still required)")
	}

	// Build a client hello with binder_len=0.
	req := buildOperatorClientHello(nil, nil)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("no-PSK + valid identity: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must be authed after no-PSK + valid identity")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil on no-PSK success")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

func TestPSKGate_NoPSKNoIdentity(t *testing.T) {
	// No PSK configured, but the payload is malformed (too short to contain a
	// valid PskAuthRequest). Decode failure → gate returns no_identity + close.
	g := newPSKGate()

	// Payload: AppKind_PskAuth + binder_len=0 (2 bytes) + role=client + then
	// nothing. The brgen decoder needs at least the ClientHello bytes after the
	// role, so truncation here causes decode error → NoIdentity.
	data := []byte{
		byte(appwire.AppKind_PskAuth),
		0x00, 0x00, // binder_len = 0
		byte(protocol.AuthRole_Client),
		// no ClientHello bytes — truncated
	}

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK {
		t.Error("isPSK: want true (it is a PskAuth kind byte)")
	}
	if !shouldClose {
		t.Error("shouldClose: want true (no identity)")
	}
	if g.Authed() {
		t.Error("gate must NOT be authed after no identity")
	}
	if accepted != nil {
		t.Error("accepted: want nil on no-identity failure")
	}
	// The response should be NoIdentity (decode failure path).
	if len(sent) >= 2 {
		status := parsePskResponse(t, sent)
		if status != protocol.PskAuthStatus_NoIdentity {
			t.Errorf("response status: got %v, want NoIdentity", status)
		}
	}
}

// The two roles every binary spoke before runner enrollment are retired: an
// old binary's hello, byte for byte, is refused no_identity, which a runner
// retries rather than exits on (cli.PskRejectedError.Retryable).
func TestPSKGate_RetiredRolesAreNoIdentity(t *testing.T) {
	rh := testRunnerHello("old-runner")
	runnerHello, err := rh.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		// [0x45] binder_len=0, role=1 (runner_legacy), RunnerHello.
		"runner_legacy": append([]byte{byte(appwire.AppKind_PskAuth), 0x00, 0x00, 0x01}, runnerHello...),
		// [0x45] binder_len=0, role=0 (client_legacy), ClientHelloLegacy{kind=cli}.
		"client_legacy": {byte(appwire.AppKind_PskAuth), 0x00, 0x00, 0x00, byte(protocol.ClientKind_Cli)},
	} {
		g := gateWithKeys(t)
		var sent []byte
		isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })
		if !isPSK || !shouldClose || accepted != nil || g.Authed() {
			t.Errorf("%s: isPSK=%v shouldClose=%v accepted=%v authed=%v, want refused", name, isPSK, shouldClose, accepted != nil, g.Authed())
		}
		if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_NoIdentity {
			t.Errorf("%s: status %v, want no_identity", name, st)
		}
	}
}

func TestPSKGate_NonPSKMessageBeforeAuth(t *testing.T) {
	g := newPSKGate()
	g.operatorPSK = []byte("s3cr3t")
	isPSK, shouldClose, accepted := g.Check(
		[]byte{byte(appwire.AppKind_TaskControl), 0x00},
		testTranscript, func([]byte) {},
	)
	if isPSK || !shouldClose {
		t.Errorf("non-PSK before auth: isPSK=%v shouldClose=%v, want false true", isPSK, shouldClose)
	}
	if accepted != nil {
		t.Error("accepted: want nil for non-PSK message")
	}
}

func TestPSKGate_AlreadyAuthed(t *testing.T) {
	// Gate that is already authed (e.g., after a successful Check).
	psk := []byte("s3cr3t")
	g := newPSKGate()
	g.operatorPSK = psk

	// First: auth it.
	req := buildOperatorClientHello(psk, testTranscript)
	data := buildPskAuthData(t, req)
	g.Check(data, testTranscript, func([]byte) {}) //nolint: errcheck

	if !g.Authed() {
		t.Fatal("gate not authed after first check")
	}

	// Second call: gate should return false,false,nil regardless of payload.
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func([]byte) {})
	if isPSK || shouldClose || accepted != nil {
		t.Errorf("authed gate: isPSK=%v shouldClose=%v accepted=%v, want false false nil",
			isPSK, shouldClose, accepted)
	}
}

// ------- Identity recording integration tests -------
// These tests wire a Dispatcher + TaskHandler / RunnerHandler and verify that
// pskDispatchIdentity causes clientKinds / principals / registry to be recorded.

// newTestTaskHandler builds a minimal TaskHandler (no board → nil OnAgentHello).
func newTestTaskHandler(tasks *TaskStore) *TaskHandler {
	return &TaskHandler{
		Tasks:    tasks,
		Registry: NewRegistry(),
	}
}

// newTestRunnerHandler builds a minimal RunnerHandler suitable for registration tests.
func newTestRunnerHandler(reg *Registry) *RunnerHandler {
	return &RunnerHandler{
		Registry: reg,
		Tasks:    NewTaskStore(),
		Now:      time.Now,
	}
}

func TestPSKDispatchIdentity_OperatorClientKindsSet(t *testing.T) {
	tasks := NewTaskStore()
	th := newTestTaskHandler(tasks)
	reg := NewRegistry()
	rh := newTestRunnerHandler(reg)

	d := &Dispatcher{
		OnTaskControl:        th.Handle,
		OnRunnerControl:      rh.Handle,
		RecordClientIdentity: th.RecordClientIdentity,
		Registry:             reg,
		Tasks:                tasks,
	}

	connIDStr := "ws:127.0.0.1:9200-1"
	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}

	req := buildOperatorClientHello(nil, nil) // no PSK; binder_len=0
	pskDispatchIdentity(d, conn, req)

	// TaskHandler must have recorded the ClientKind.
	th.clientKindsMu.Lock()
	kind := th.clientKinds[connIDStr]
	th.clientKindsMu.Unlock()

	if kind != protocol.ClientKind_Cli {
		t.Errorf("clientKind: got %v, want Cli", kind)
	}

	// With RecordClientIdentity wired, pskDispatchIdentity must NOT emit a
	// TaskControlResponse{ClientHello} — only the gate's PskAuthResponse is sent.
	sent := conn.Sent()
	for _, msg := range sent {
		if len(msg) > 0 && appwire.AppKind(msg[0]) == appwire.AppKind_TaskControl {
			t.Errorf("client must NOT receive a TaskControl response from the PSK dispatch path; got %d bytes", len(msg))
		}
	}
}

func TestPSKDispatchIdentity_AgentPrincipalSet(t *testing.T) {
	board := newTestBoard(t)

	connIDStr := "ws:127.0.0.1:9201-1"
	protoRID := makeProtoRunnerID(t, connIDStr)
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0x11, 0x22, 0x33}
	var ticket [16]byte
	for i := range ticket {
		ticket[i] = byte(i + 0x10)
	}
	board.Registry().Register(protoRID, protoTID, ticket)

	tasks := NewTaskStore()
	s := &Server{Board: board, tasks: tasks}
	th := &TaskHandler{
		Tasks:    tasks,
		Registry: NewRegistry(),
		OnAgentHello: func(conn ConnHandle, info *protocol.AgentInfo) protocol.ClientHelloStatus {
			return s.establishAgentIdentity(conn, info)
		},
	}

	reg := NewRegistry()
	rh := newTestRunnerHandler(reg)

	d := &Dispatcher{
		OnTaskControl:        th.Handle,
		OnRunnerControl:      rh.Handle,
		RecordClientIdentity: th.RecordClientIdentity,
		Registry:             reg,
		Tasks:                tasks,
	}

	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}

	req := buildAgentBinderReq(ticket, nil, protoRID, protoTID)
	pskDispatchIdentity(d, conn, req)

	// principals must be set.
	th.clientKindsMu.Lock()
	principal := th.principals[connIDStr]
	th.clientKindsMu.Unlock()

	if principal.Id != protoTID.Id {
		t.Errorf("principal TaskID: got %x, want %x", principal.Id, protoTID.Id)
	}

	// With RecordClientIdentity wired, no TaskControl response must be emitted.
	sent := conn.Sent()
	for _, msg := range sent {
		if len(msg) > 0 && appwire.AppKind(msg[0]) == appwire.AppKind_TaskControl {
			t.Errorf("agent client must NOT receive a TaskControl response from the PSK dispatch path; got %d bytes", len(msg))
		}
	}
}

func TestPSKDispatchIdentity_BadTicketNoPrincipal(t *testing.T) {
	// This test verifies that a bad-ticket agent does NOT have a principal recorded.
	// In production the gate rejects at step 4, so pskDispatchIdentity is never
	// called. Here we verify the gate correctly returns no accepted request.
	var protoRID protocol.RunnerID
	protoRID.Id = [16]byte{1}
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0xEE, 0xFF}
	g := agentGate(protoRID, protoTID, [16]byte{0x01})
	g.operatorPSK = []byte("s3cr3t")

	var badTicket [16]byte
	req := buildAgentBinderReq(badTicket, testTranscript, protoRID, protoTID)
	data := buildPskAuthData(t, req)

	var sent []byte
	_, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	// Gate must close + return nil (no principal recorded).
	if !shouldClose {
		t.Error("shouldClose: want true for bad-ticket agent")
	}
	if accepted != nil {
		t.Error("accepted: want nil — bad-ticket agent must NOT have identity dispatched")
	}
	if g.Authed() {
		t.Error("gate must NOT be authed after bad-ticket agent")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_BadTicket {
		t.Errorf("response status: got %v, want BadTicket", status)
	}
}

func TestPSKDispatchIdentity_RunnerRegistered(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	rh := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
	}

	d := &Dispatcher{
		OnRunnerControl: rh.Handle,
		Registry:        reg,
		Tasks:           tasks,
	}

	connIDStr := "ws:127.0.0.1:9202-1"
	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}

	rh2 := protocol.RunnerHello{Version: 1, MaxTasks: 3}
	rh2.SetHostname([]byte("my-runner"))

	req := buildRunnerDispatchReq(rh2)
	pskDispatchIdentity(d, conn, req)

	// Registry must have the runner registered.
	entry, ok := reg.Get(objproto.MustParseConnectionID(connIDStr))
	if !ok {
		t.Fatal("runner must be registered in Registry after RunnerHello dispatch")
	}
	if entry.Hostname != "my-runner" {
		t.Errorf("runner hostname: got %q, want %q", entry.Hostname, "my-runner")
	}
	if entry.MaxTasks != 3 {
		t.Errorf("runner MaxTasks: got %d, want 3", entry.MaxTasks)
	}
}

// TestPSKDispatchIdentity_ClientReceivesNoTaskControlResponse verifies that when
// RecordClientIdentity is wired (production path), pskDispatchIdentity does NOT send
// any TaskControlResponse{ClientHello} to the client connection — the gate's own
// PskAuthResponse is the sole client handshake response.
func TestPSKDispatchIdentity_ClientReceivesNoTaskControlResponse(t *testing.T) {
	tasks := NewTaskStore()
	th := newTestTaskHandler(tasks)
	reg := NewRegistry()
	rh := newTestRunnerHandler(reg)

	d := &Dispatcher{
		OnTaskControl:        th.Handle,
		OnRunnerControl:      rh.Handle,
		RecordClientIdentity: th.RecordClientIdentity,
		Registry:             reg,
		Tasks:                tasks,
	}

	connIDStr := "ws:127.0.0.1:9210-1"
	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}

	req := buildOperatorClientHello(nil, nil) // operator (kind=Cli)
	pskDispatchIdentity(d, conn, req)

	// The conn must have received zero messages — pskDispatchIdentity's client
	// branch calls RecordClientIdentity (no send) and returns.
	sent := conn.Sent()
	for _, msg := range sent {
		if len(msg) > 0 && appwire.AppKind(msg[0]) == appwire.AppKind_TaskControl {
			t.Fatalf("pskDispatchIdentity sent a TaskControl message (%d bytes) to client — redundant response not suppressed", len(msg))
		}
	}

	// Identity must still be recorded.
	th.clientKindsMu.Lock()
	kind := th.clientKinds[connIDStr]
	th.clientKindsMu.Unlock()
	if kind != protocol.ClientKind_Cli {
		t.Errorf("clientKind: got %v, want Cli", kind)
	}
}

// TestPSKDispatchIdentity_RunnerReceivesRunnerHelloResponse verifies that the
// runner path still emits a RunnerHelloResponse (carried in [0x43]+RunnerRequest)
// via re-dispatch. The runner MUST receive this response (it carries YourRunnerId).
func TestPSKDispatchIdentity_RunnerReceivesRunnerHelloResponse(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	rh := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
	}

	d := &Dispatcher{
		OnRunnerControl: rh.Handle,
		Registry:        reg,
		Tasks:           tasks,
	}

	connIDStr := "ws:127.0.0.1:9211-1"
	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}

	rh2 := protocol.RunnerHello{Version: 1, MaxTasks: 2}
	rh2.SetHostname([]byte("runner-rhr-test"))

	req := buildRunnerDispatchReq(rh2)
	pskDispatchIdentity(d, conn, req)

	// The runner conn must have received a RunnerControl message
	// (the RunnerHelloResponse from RunnerHandler).
	sent := conn.Sent()
	var gotRunnerControl bool
	for _, msg := range sent {
		if len(msg) > 0 && appwire.AppKind(msg[0]) == appwire.AppKind_RunnerControl {
			gotRunnerControl = true
		}
	}
	if !gotRunnerControl {
		t.Error("runner must receive a RunnerControl message (RunnerHelloResponse) from pskDispatchIdentity")
	}

	// Registry must be populated.
	entry, ok := reg.Get(objproto.MustParseConnectionID(connIDStr))
	if !ok {
		t.Fatal("runner must be registered in Registry after dispatch")
	}
	if entry.Hostname != "runner-rhr-test" {
		t.Errorf("runner hostname: got %q, want runner-rhr-test", entry.Hostname)
	}
}

// TestPSKGate_NoPSKConfig_RequiresHandshake verifies that a no-PSK gate does NOT
// pre-authenticate: every connection (even with nil PSK) must complete the identity
// handshake. This replaces the old test that checked authed=true on newPSKGate().
func TestPSKGate_NoPSKConfig_RequiresHandshake(t *testing.T) {
	g := newPSKGate()
	if g.Authed() {
		t.Fatal("gate with nil PSK must NOT be pre-authenticated; identity handshake is required")
	}

	// A non-PSK message before the handshake must be rejected.
	isPSK, shouldClose, accepted := g.Check(
		[]byte{byte(appwire.AppKind_TaskControl), 0x00},
		testTranscript, func([]byte) {},
	)
	if isPSK || !shouldClose {
		t.Errorf("non-PSK before auth (no-PSK config): isPSK=%v shouldClose=%v, want false true", isPSK, shouldClose)
	}
	if accepted != nil {
		t.Error("accepted: want nil for non-PSK message")
	}
}

// TestPSKGate_NoPSKConfig_AcceptsHandshake verifies that a no-PSK gate accepts a
// PskAuthRequest with binder_len=0 (dev mode identity-only handshake).
func TestPSKGate_NoPSKConfig_AcceptsHandshake(t *testing.T) {
	g := newPSKGate()

	req := buildOperatorClientHello(nil, nil) // binder_len=0
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })
	if !isPSK || shouldClose {
		t.Errorf("no-PSK handshake: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must be authed after valid no-PSK handshake")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

// TestPSKGate_AgentboardFullValidation tests the gate + full agentboard ticket
// validation path (using a real agentboard.Board), parallel to
// TestEstablishAgentIdentity_ValidTicket but exercising the gate layer.
func TestPSKGate_AgentboardFullValidation(t *testing.T) {
	board := agentboard.New(agentboard.Config{
		RingN:      8,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 1024,
	})
	t.Cleanup(func() { board.Close() })

	connIDStr := "ws:127.0.0.1:9203-1"
	protoRID := makeProtoRunnerID(t, connIDStr)
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0x55, 0x66}
	var ticket [16]byte
	for i := range ticket {
		ticket[i] = byte(i + 0x20)
	}
	board.Registry().Register(protoRID, protoTID, ticket)

	g := newPSKGate()
	g.operatorPSK = []byte("s3cr3t-full")
	// The same ticket lookup as Server.handleConnection wires.
	g.TicketFor = board.Registry().Ticket

	req := buildAgentBinderReq(ticket, testTranscript, protoRID, protoTID)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("agentboard full valid: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must be authed")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

// TestPSKGate_AgentboardBadTicket_NoPrincipal exercises the gate with a real
// agentboard where the ticket is wrong. Accepted must be nil — no principal recorded.
func TestPSKGate_AgentboardBadTicket_NoPrincipal(t *testing.T) {
	board := agentboard.New(agentboard.Config{
		RingN:      8,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 1024,
	})
	t.Cleanup(func() { board.Close() })

	connIDStr := "ws:127.0.0.1:9204-1"
	protoRID := makeProtoRunnerID(t, connIDStr)
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0x77, 0x88}
	var goodTicket [16]byte
	goodTicket[0] = 0xAA
	board.Registry().Register(protoRID, protoTID, goodTicket)

	g := newPSKGate()
	g.operatorPSK = []byte("s3cr3t-full")
	g.TicketFor = board.Registry().Ticket

	var badTicket [16]byte // all zeros — wrong
	req := buildAgentBinderReq(badTicket, testTranscript, protoRID, protoTID)
	data := buildPskAuthData(t, req)

	var sent []byte
	_, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !shouldClose {
		t.Error("shouldClose: want true for bad-ticket agent")
	}
	if g.Authed() {
		t.Error("gate must NOT be authed after bad ticket")
	}
	if accepted != nil {
		t.Error("accepted: want nil — bad-ticket agent must NOT get identity dispatched")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_BadTicket {
		t.Errorf("response status: got %v, want BadTicket", status)
	}
}

// parsePskResponseFull decodes the whole response, signature included.
func parsePskResponseFull(t *testing.T, sent []byte) protocol.PskAuthResponse {
	t.Helper()
	if len(sent) < 2 || appwire.AppKind(sent[0]) != appwire.AppKind_PskAuth {
		t.Fatalf("not a PskAuth response: %v", sent)
	}
	var resp protocol.PskAuthResponse
	if err := resp.DecodeExact(sent[1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// Every status Check can send is signed, rejections included (spec D4).
func TestPSKGate_SignsEveryStatus(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	psk := []byte("s3cr3t")
	cases := map[protocol.PskAuthStatus][]byte{
		protocol.PskAuthStatus_Ok:         buildPskAuthData(t, buildOperatorClientHello(psk, testTranscript)),
		protocol.PskAuthStatus_BadPsk:     buildPskAuthData(t, buildOperatorClientHello([]byte("wrong"), testTranscript)),
		protocol.PskAuthStatus_NoIdentity: {byte(appwire.AppKind_PskAuth), 0xff},
		// No TicketFor wired: every role=client agent is refused.
		protocol.PskAuthStatus_BadTicket: buildPskAuthData(t, buildAgentBinderReq([16]byte{}, testTranscript, protocol.RunnerID{}, protocol.TaskID{})),
	}
	for want, data := range cases {
		g := newPSKGate()
		g.operatorPSK = psk
		g.serverKey = priv
		var sent []byte
		g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })
		resp := parsePskResponseFull(t, sent)
		if resp.Status != want {
			t.Errorf("status = %v, want %v", resp.Status, want)
			continue
		}
		if err := cli.VerifyServerAuth(pub, testTranscript, &resp); err != nil {
			t.Errorf("%v: %v", want, err)
		}
	}
}

// server.New gives every server a key, so the gate it builds always signs.
func TestNew_AlwaysHasServerKey(t *testing.T) {
	s := New(Config{})
	if len(s.cfg.ServerKey) != ed25519.PrivateKeySize {
		t.Fatalf("server.New left ServerKey unset (len %d)", len(s.cfg.ServerKey))
	}
}
