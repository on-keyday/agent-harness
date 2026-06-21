package server

import (
	"bytes"
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

// buildAgentClientHello builds a PskAuthRequest for an in-task agent.
func buildAgentClientHello(psk, transcript []byte, info protocol.AgentInfo) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
	if len(psk) > 0 {
		binder, _ := cli.ComputePSKBinder(psk, transcript)
		req.SetBinder(binder)
	}
	hello := protocol.ClientHello{Kind: protocol.ClientKind_Agent}
	hello.SetAgentInfo(info)
	req.SetClientHello(hello)
	return req
}

// buildRunnerHelloReq builds a PskAuthRequest for a runner.
func buildRunnerHelloReq(psk, transcript []byte, rh protocol.RunnerHello) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Runner}
	if len(psk) > 0 {
		binder, _ := cli.ComputePSKBinder(psk, transcript)
		req.SetBinder(binder)
	}
	req.SetRunnerHello(rh)
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
	g := newPSKGate(psk)
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

// Operator-secret model: when operatorPSK is configured, operator-surface
// connections (kind=cli/tui/webui) must prove operatorPSK via the binder. An
// in-task agent holds only the connect psk, so a binder it computes cannot
// authenticate as operator — this closes the kind=Client → operator escalation.

func TestPSKGate_OperatorClientRejectsConnectPSK(t *testing.T) {
	connectPSK := []byte("connect-secret")   // what runners inject into agents
	operatorPSK := []byte("operator-secret") // operator-only, never given to agents
	g := newPSKGate(connectPSK)
	g.operatorPSK = operatorPSK

	// Simulate an in-task agent: it has only the connect psk but claims kind=Cli
	// to try to be treated as operator. Its binder is over the connect psk.
	req := buildOperatorClientHello(connectPSK, testTranscript)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || !shouldClose {
		t.Errorf("operator client with connect psk: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() {
		t.Error("gate must NOT auth an operator client that only proved the connect psk")
	}
	if accepted != nil {
		t.Error("accepted: want nil (escalation must be rejected)")
	}
	if status := parsePskResponse(t, sent); status != protocol.PskAuthStatus_BadPsk {
		t.Errorf("response status: got %v, want BadPsk", status)
	}
}

func TestPSKGate_OperatorClientAcceptsOperatorPSK(t *testing.T) {
	connectPSK := []byte("connect-secret")
	operatorPSK := []byte("operator-secret")
	g := newPSKGate(connectPSK)
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

func TestPSKGate_AgentStillUsesConnectPSKWhenOperatorPSKSet(t *testing.T) {
	connectPSK := []byte("connect-secret")
	operatorPSK := []byte("operator-secret")
	g := newPSKGate(connectPSK)
	g.operatorPSK = operatorPSK

	var ticket [16]byte
	for i := range ticket {
		ticket[i] = byte(i + 1)
	}
	g.ValidateTicket = func(info *protocol.AgentInfo) protocol.PskAuthStatus {
		if bytes.Equal(info.AuthTicket[:], ticket[:]) {
			return protocol.PskAuthStatus_Ok
		}
		return protocol.PskAuthStatus_BadTicket
	}

	var protoRID protocol.RunnerID
	protoRID.SetTransport([]byte("ws"))
	protoRID.SetIpAddr([]byte{127, 0, 0, 1})
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0xAA, 0xBB}
	info := protocol.AgentInfo{RunnerId: protoRID, TaskId: protoTID, AuthTicket: ticket}
	info.SetHostname([]byte("testhost"))

	// Agent proves the CONNECT psk (not the operator psk) + a valid ticket.
	req := buildAgentClientHello(connectPSK, testTranscript, info)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("agent with connect psk: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() || accepted == nil {
		t.Error("agent proving connect psk + valid ticket must still be accepted when operatorPSK is set")
	}
}

func TestPSKGate_ValidBinderAgentValidTicket(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate(psk)

	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0xAA, 0xBB}
	var ticket [16]byte
	for i := range ticket {
		ticket[i] = byte(i + 1)
	}

	// Wire a ValidateTicket callback that succeeds.
	g.ValidateTicket = func(info *protocol.AgentInfo) protocol.PskAuthStatus {
		if bytes.Equal(info.AuthTicket[:], ticket[:]) {
			return protocol.PskAuthStatus_Ok
		}
		return protocol.PskAuthStatus_BadTicket
	}

	var protoRID protocol.RunnerID
	protoRID.SetTransport([]byte("ws"))
	protoRID.SetIpAddr([]byte{127, 0, 0, 1})

	info := protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: ticket,
	}
	info.SetHostname([]byte("testhost"))

	req := buildAgentClientHello(psk, testTranscript, info)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("agent valid ticket: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must be authed after valid agent ticket")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

func TestPSKGate_ValidBinderAgentBadTicket(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate(psk)

	// ValidateTicket always fails.
	g.ValidateTicket = func(*protocol.AgentInfo) protocol.PskAuthStatus {
		return protocol.PskAuthStatus_BadTicket
	}

	var protoRID protocol.RunnerID
	protoRID.SetTransport([]byte("ws"))
	protoRID.SetIpAddr([]byte{127, 0, 0, 1})
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0xCC, 0xDD}
	var badTicket [16]byte
	badTicket[0] = 0xFF

	info := protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: badTicket,
	}
	info.SetHostname([]byte("testhost"))

	req := buildAgentClientHello(psk, testTranscript, info)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || !shouldClose {
		t.Errorf("bad ticket: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() {
		t.Error("gate must NOT be authed after bad ticket")
	}
	if accepted != nil {
		t.Error("accepted: want nil on bad-ticket failure")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_BadTicket {
		t.Errorf("response status: got %v, want BadTicket", status)
	}
}

func TestPSKGate_BadBinder(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate(psk)

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
	g := newPSKGate(psk)

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
	g := newPSKGate(nil)
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
	g := newPSKGate(nil)

	// Payload: AppKind_PskAuth + binder_len=0 (2 bytes) + role=0 (client) +
	// then nothing. The brgen decoder needs at least the ClientHello bytes after
	// the role, so truncation here causes decode error → NoIdentity.
	data := []byte{
		byte(appwire.AppKind_PskAuth),
		0x00, 0x00, // binder_len = 0
		0x00, // role = AuthRole_Client
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

func TestPSKGate_RunnerRole(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate(psk)

	rh := protocol.RunnerHello{Version: 1, MaxTasks: 2}
	rh.SetHostname([]byte("runner-host"))

	req := buildRunnerHelloReq(psk, testTranscript, rh)
	data := buildPskAuthData(t, req)

	var sent []byte
	isPSK, shouldClose, accepted := g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })

	if !isPSK || shouldClose {
		t.Errorf("runner role: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must be authed after valid runner hello")
	}
	if accepted == nil {
		t.Fatal("accepted: want non-nil")
	}
	if accepted.RunnerHello() == nil {
		t.Error("accepted.RunnerHello(): want non-nil")
	}
	status := parsePskResponse(t, sent)
	if status != protocol.PskAuthStatus_Ok {
		t.Errorf("response status: got %v, want Ok", status)
	}
}

func TestPSKGate_NonPSKMessageBeforeAuth(t *testing.T) {
	g := newPSKGate([]byte("s3cr3t"))
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
	g := newPSKGate(psk)

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
			return clientHelloStatusFromBoard(s.establishAgentIdentity(conn, info))
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

	info := protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: ticket,
	}
	info.SetHostname([]byte("testhost"))

	req := buildAgentClientHello(nil, nil, info) // no PSK
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
	psk := []byte("s3cr3t")
	g := newPSKGate(psk)
	g.ValidateTicket = func(*protocol.AgentInfo) protocol.PskAuthStatus {
		return protocol.PskAuthStatus_BadTicket
	}

	var protoRID protocol.RunnerID
	protoRID.SetTransport([]byte("ws"))
	protoRID.SetIpAddr([]byte{127, 0, 0, 1})
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0xEE, 0xFF}
	var badTicket [16]byte

	info := protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: badTicket,
	}
	info.SetHostname([]byte("testhost"))

	req := buildAgentClientHello(psk, testTranscript, info)
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

	req := buildRunnerHelloReq(nil, nil, rh2) // no PSK
	pskDispatchIdentity(d, conn, req)

	// Registry must have the runner registered.
	entry, ok := reg.Get(connIDStr)
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

	req := buildRunnerHelloReq(nil, nil, rh2)
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
	entry, ok := reg.Get(connIDStr)
	if !ok {
		t.Fatal("runner must be registered in Registry after dispatch")
	}
	if entry.Hostname != "runner-rhr-test" {
		t.Errorf("runner hostname: got %q, want runner-rhr-test", entry.Hostname)
	}
}

// TestPSKGate_NoPSKConfig_RequiresHandshake verifies that a no-PSK gate does NOT
// pre-authenticate: every connection (even with nil PSK) must complete the identity
// handshake. This replaces the old test that checked authed=true on newPSKGate(nil).
func TestPSKGate_NoPSKConfig_RequiresHandshake(t *testing.T) {
	g := newPSKGate(nil)
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
	g := newPSKGate(nil)

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

	psk := []byte("s3cr3t-full")
	g := newPSKGate(psk)
	// Wire the same ticket validation logic as Server.handleConnection.
	g.ValidateTicket = func(info *protocol.AgentInfo) protocol.PskAuthStatus {
		rid := boardRunnerIDFromProto(info.RunnerId)
		tid := boardTaskIDFromProto(info.TaskId)
		s := board.Registry().Validate(rid, tid, info.AuthTicket)
		if s == agentboard.HelloStatusOk {
			return protocol.PskAuthStatus_Ok
		}
		return protocol.PskAuthStatus_BadTicket
	}

	info := protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: ticket,
	}
	info.SetHostname([]byte("testhost"))

	req := buildAgentClientHello(psk, testTranscript, info)
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

	psk := []byte("s3cr3t-full")
	g := newPSKGate(psk)
	g.ValidateTicket = func(info *protocol.AgentInfo) protocol.PskAuthStatus {
		rid := boardRunnerIDFromProto(info.RunnerId)
		tid := boardTaskIDFromProto(info.TaskId)
		s := board.Registry().Validate(rid, tid, info.AuthTicket)
		if s == agentboard.HelloStatusOk {
			return protocol.PskAuthStatus_Ok
		}
		return protocol.PskAuthStatus_BadTicket
	}

	var badTicket [16]byte // all zeros — wrong
	info := protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: badTicket,
	}
	info.SetHostname([]byte("testhost"))

	req := buildAgentClientHello(psk, testTranscript, info)
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
