package server

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// makeTestServer builds a minimal Server with an empty TaskStore, Registry,
// and activeConns map, suitable for ConnList unit tests.
func makeTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		registry:    NewRegistry(),
		tasks:       NewTaskStore(),
		activeConns: make(map[objproto.ConnectionID]streamingConn),
	}
	s.taskHandler = &TaskHandler{
		Tasks:    s.tasks,
		Registry: s.registry,
	}
	s.taskHandler.ConnListFn = s.ConnList
	return s
}

// addActiveConn injects a fake streamingConn into s.activeConns.
func addActiveConn(s *Server, cidStr string, since time.Time) objproto.ConnectionID {
	cid := objproto.MustParseConnectionID(cidStr)
	sc := streamingConn{
		Connection:     &fakeRawConn{cid: cid},
		connectedSince: since,
	}
	s.activeConnsMu.Lock()
	s.activeConns[cid] = sc
	s.activeConnsMu.Unlock()
	return cid
}

// fakeRawConn is a minimal objproto.Connection stub for ConnList tests.
// Only ConnectionID() is used; all other methods return zero values or no-op.
type fakeRawConn struct {
	cid objproto.ConnectionID
}

func (f *fakeRawConn) ConnectionID() objproto.ConnectionID { return f.cid }
func (f *fakeRawConn) SetName(_ string)                    {}
func (f *fakeRawConn) Name() string                        { return "" }
func (f *fakeRawConn) ConsumePacketNumber() objproto.PacketNumber {
	return 0
}
func (f *fakeRawConn) SendMessageWithPacketNumber(_ []byte, _ objproto.PacketNumber) (int, objproto.PacketNumber, error) {
	return 0, 0, nil
}
func (f *fakeRawConn) SendMessage(_ []byte) (int, objproto.PacketNumber, error) { return 0, 0, nil }
func (f *fakeRawConn) ReceiveMessage() (*objproto.Message, error)               { return nil, nil }
func (f *fakeRawConn) ReceiveMessageTimeout(_ context.Context, _ time.Duration) (*objproto.Message, error) {
	return nil, nil
}
func (f *fakeRawConn) ReceiveMessageContext(_ context.Context) (*objproto.Message, error) {
	return nil, nil
}
func (f *fakeRawConn) GetTranscript() []byte  { return nil }
func (f *fakeRawConn) ConnectedAt() time.Time { return time.Time{} }
func (f *fakeRawConn) LastTime() time.Time    { return time.Time{} }
func (f *fakeRawConn) Close() error           { return nil }
func (f *fakeRawConn) IsActive() bool         { return true }
func (f *fakeRawConn) Done() <-chan struct{} {
	ch := make(chan struct{})
	return ch
}
func (f *fakeRawConn) RehandshakeForProxy(_ []byte, _ *objproto.Handshake) (*objproto.ChanWithTimeout[objproto.Connection], error) {
	return nil, nil
}
func (f *fakeRawConn) IsProxied() bool { return false }

// TestConnList_JoinAndRoles verifies that ConnList correctly derives ConnRole
// from the identity map and runner registry:
//
//   - A CLI client → ConnRole_Cli, Identified=true, zero principal
//   - An agent client → ConnRole_Agent, Identified=true, non-zero principal
//   - A registered runner → ConnRole_Runner, Identified=true, zero principal
//   - An unidentified conn → ConnRole_Unspecified, Identified=false, zero principal
func TestConnList_JoinAndRoles(t *testing.T) {
	s := makeTestServer(t)
	now := time.Now()

	// (i) CLI conn — record ClientHello kind=Cli
	cliCID := addActiveConn(s, "ws:127.0.0.1:9100-1", now)
	s.taskHandler.clientKinds = map[string]protocol.ClientKind{
		cliCID.String(): protocol.ClientKind_Cli,
	}

	// (ii) Agent conn — record ClientHello kind=Agent with a principal task
	agentCID := addActiveConn(s, "ws:127.0.0.1:9100-2", now)
	var principalID protocol.TaskID
	copy(principalID.Id[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	agentTaskID := hex.EncodeToString(principalID.Id[:])
	s.tasks.Create("/repo", "agent-task", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	// Override the auto-generated ID with our known principal:
	s.taskHandler.clientKinds[agentCID.String()] = protocol.ClientKind_Agent
	if s.taskHandler.principals == nil {
		s.taskHandler.principals = make(map[string]protocol.TaskID)
	}
	s.taskHandler.principals[agentCID.String()] = principalID
	_ = agentTaskID

	// (iii) Runner conn — register in the runner registry
	runnerCID := addActiveConn(s, "ws:127.0.0.1:9100-3", now)
	s.registry.Add(&RunnerEntry{
		ID:           runnerCID.String(),
		Hostname:     "runner-host",
		AllowedRoots: []string{"/"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  now,
		LastSeen:     now,
		Conn:         stubConn{},
	})

	// (iv) Unidentified conn — no ClientHello, not in runner registry
	_ = addActiveConn(s, "ws:127.0.0.1:9100-4", now)

	infos := s.ConnList(protocol.TaskID{}, true) // operator + globalView
	if len(infos) != 4 {
		t.Fatalf("expected 4 ConnInfo entries, got %d", len(infos))
	}

	// Build a map by CID string for assertion order-independence.
	byRole := make(map[protocol.ConnRole][]protocol.ConnInfo)
	for _, info := range infos {
		byRole[info.Role] = append(byRole[info.Role], info)
	}

	// CLI
	cliInfos := byRole[protocol.ConnRole_Cli]
	if len(cliInfos) != 1 {
		t.Errorf("expected 1 Cli role, got %d", len(cliInfos))
	} else if !cliInfos[0].Identified() {
		t.Errorf("Cli conn should be Identified=true")
	} else if cliInfos[0].PrincipalTask.Id != ([16]byte{}) {
		t.Errorf("Cli conn should have zero principal task")
	}

	// Agent
	agentInfos := byRole[protocol.ConnRole_Agent]
	if len(agentInfos) != 1 {
		t.Errorf("expected 1 Agent role, got %d", len(agentInfos))
	} else {
		ai := agentInfos[0]
		if !ai.Identified() {
			t.Errorf("Agent conn should be Identified=true")
		}
		if ai.PrincipalTask.Id != principalID.Id {
			t.Errorf("Agent conn principal mismatch: got %x, want %x", ai.PrincipalTask.Id, principalID.Id)
		}
	}

	// Runner
	runnerInfos := byRole[protocol.ConnRole_Runner]
	if len(runnerInfos) != 1 {
		t.Errorf("expected 1 Runner role, got %d", len(runnerInfos))
	} else if !runnerInfos[0].Identified() {
		t.Errorf("Runner conn should be Identified=true")
	}

	// Unspecified
	unspecInfos := byRole[protocol.ConnRole_Unspecified]
	if len(unspecInfos) != 1 {
		t.Errorf("expected 1 Unspecified role, got %d", len(unspecInfos))
	} else {
		u := unspecInfos[0]
		if u.Identified() {
			t.Errorf("Unidentified conn should have Identified=false")
		}
		if u.PrincipalTask.Id != ([16]byte{}) {
			t.Errorf("Unidentified conn should have zero principal task")
		}
	}
}

// TestConnList_SubtreeGating verifies that a confined viewer (hasInfoGlobal=false,
// non-zero viewerTaskID) sees only agent conns whose principal is in its subtree.
//
// Setup: parent task P, child task C (creator=P), grandchild task G (creator=C).
// Viewer is P. Conns:
//   - agentP: principal=P (viewer itself) → visible
//   - agentC: principal=C (direct child) → visible
//   - agentG: principal=G (grandchild) → visible
//   - agentOther: principal=some unrelated task → NOT visible
//   - cliConn: role=Cli, no principal → NOT visible to confined viewer
//   - unidentConn: no identity → NOT visible to confined viewer
func TestConnList_SubtreeGating(t *testing.T) {
	s := makeTestServer(t)
	now := time.Now()

	// Create tasks P, C, G in TaskStore.
	var pidTask, cidTask, gidTask, otherTask protocol.TaskID
	pHex := s.tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	copyHexToID(t, pHex, &pidTask)
	cHex := s.tasks.Create("/r", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, pidTask, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, "")
	copyHexToID(t, cHex, &cidTask)
	gHex := s.tasks.Create("/r", "g", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, cidTask, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, "")
	copyHexToID(t, gHex, &gidTask)
	otherHex := s.tasks.Create("/r", "other", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	copyHexToID(t, otherHex, &otherTask)

	// Agent conn with principal=P
	agentPCID := addActiveConn(s, "ws:127.0.0.1:9200-1", now)
	recordAgent(s, agentPCID.String(), pidTask)

	// Agent conn with principal=C
	agentCCID := addActiveConn(s, "ws:127.0.0.1:9200-2", now)
	recordAgent(s, agentCCID.String(), cidTask)

	// Agent conn with principal=G
	agentGCID := addActiveConn(s, "ws:127.0.0.1:9200-3", now)
	recordAgent(s, agentGCID.String(), gidTask)

	// Agent conn with principal=other (unrelated)
	agentOtherCID := addActiveConn(s, "ws:127.0.0.1:9200-4", now)
	recordAgent(s, agentOtherCID.String(), otherTask)
	_ = agentOtherCID

	// CLI conn (no principal, not an agent)
	cliCID := addActiveConn(s, "ws:127.0.0.1:9200-5", now)
	if s.taskHandler.clientKinds == nil {
		s.taskHandler.clientKinds = make(map[string]protocol.ClientKind)
	}
	s.taskHandler.clientKinds[cliCID.String()] = protocol.ClientKind_Cli

	// Unidentified conn
	_ = addActiveConn(s, "ws:127.0.0.1:9200-6", now)

	// Call ConnList as viewer=P with hasInfoGlobal=false.
	visible := s.ConnList(pidTask, false)

	// Must see agentP, agentC, agentG = 3 entries.
	if len(visible) != 3 {
		t.Fatalf("confined viewer expected 3 visible conns (P, C, G subtree), got %d", len(visible))
	}
	seenP, seenC, seenG := false, false, false
	for _, info := range visible {
		if info.Role != protocol.ConnRole_Agent {
			t.Errorf("confined view: expected only Agent roles, got %v", info.Role)
		}
		switch info.PrincipalTask.Id {
		case pidTask.Id:
			seenP = true
		case cidTask.Id:
			seenC = true
		case gidTask.Id:
			seenG = true
		default:
			t.Errorf("unexpected principal %x in confined view", info.PrincipalTask.Id)
		}
	}
	if !seenP {
		t.Errorf("confined view: missing agent with principal=P")
	}
	if !seenC {
		t.Errorf("confined view: missing agent with principal=C")
	}
	if !seenG {
		t.Errorf("confined view: missing agent with principal=G")
	}

	// Global view (operator) sees all 6.
	all := s.ConnList(protocol.TaskID{}, true)
	if len(all) != 6 {
		t.Fatalf("global view expected 6 conns, got %d", len(all))
	}
}

// TestHandleListConns_StreamPattern verifies that the list_conns TaskControl
// handler allocates a send-stream, encodes ConnListResultBody, and returns a
// ConnListResult{StreamId} in the response — mirroring the handleList shape.
func TestHandleListConns_StreamPattern(t *testing.T) {
	s := makeTestServer(t)
	now := time.Now()

	// Add one identified CLI conn.
	cliCID := addActiveConn(s, "ws:127.0.0.1:9300-1", now)
	if s.taskHandler.clientKinds == nil {
		s.taskHandler.clientKinds = make(map[string]protocol.ClientKind)
	}
	s.taskHandler.clientKinds[cliCID.String()] = protocol.ClientKind_Cli

	// Wire ConnListFn (normally done by Server.New; makeTestServer does it too).

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9300-99")}
	fc.nextSendStreamID = 77

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListConns}
	req.SetListConns(protocol.ConnListQuery{})

	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	s.taskHandler.Handle(fc, payload)

	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}
	msg := fc.sent[0]
	if len(msg) < 2 || appwire.AppKind(msg[0]) != appwire.AppKind_TaskControl {
		t.Fatalf("unexpected leading byte %v", msg[0])
	}
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(msg[1:]); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_ListConns {
		t.Errorf("expected Kind=ListConns, got %v", resp.Kind)
	}
	lr := resp.ListConns()
	if lr == nil {
		t.Fatal("expected non-nil ListConns() in response")
	}
	if lr.StreamId != 77 {
		t.Errorf("expected StreamId=77, got %d", lr.StreamId)
	}

	if len(fc.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(fc.sendStreams))
	}
	ss := fc.sendStreams[0]
	if !ss.eofSent {
		t.Errorf("expected EOF on list_conns stream")
	}
	var body protocol.ConnListResultBody
	if err := body.DecodeExact(ss.bytes); err != nil {
		t.Fatalf("decode ConnListResultBody (%d bytes): %v", len(ss.bytes), err)
	}
	// The handler's conn (fc at ws:127.0.0.1:9300-99) is NOT in activeConns;
	// only the CLI conn at 9300-1 is in activeConns. Operator caller → global view.
	if body.ConnsLen != 1 {
		t.Errorf("expected 1 conn in body, got %d", body.ConnsLen)
	}
	if len(body.Conns) == 1 && body.Conns[0].Role != protocol.ConnRole_Cli {
		t.Errorf("expected Cli role, got %v", body.Conns[0].Role)
	}
}

// --- helpers ---

func copyHexToID(t *testing.T, hexStr string, out *protocol.TaskID) {
	t.Helper()
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatalf("copyHexToID: %v", err)
	}
	copy(out.Id[:], b)
}

func recordAgent(s *Server, cidStr string, principal protocol.TaskID) {
	if s.taskHandler.clientKinds == nil {
		s.taskHandler.clientKinds = make(map[string]protocol.ClientKind)
	}
	s.taskHandler.clientKinds[cidStr] = protocol.ClientKind_Agent
	if s.taskHandler.principals == nil {
		s.taskHandler.principals = make(map[string]protocol.TaskID)
	}
	s.taskHandler.principals[cidStr] = principal
}
