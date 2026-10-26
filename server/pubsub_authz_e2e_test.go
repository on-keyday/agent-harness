package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/pubsub"
	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// pubsubAuthzServer runs a real server, so every JOIN and write below goes
// through handleConnection — where the per-connection role is known — and not
// through a helper a test could call while the wiring stayed broken.
type pubsubAuthzServer struct {
	s      *Server
	cid    objproto.ConnectionID
	victim string
}

func startPubsubAuthzServer(t *testing.T) (context.Context, *pubsubAuthzServer) {
	t.Helper()
	for _, key := range []string{"HARNESS_TASK_ID", "HARNESS_RUNNER_ID", "HARNESS_AUTH_TICKET", "HARNESS_PROXY_VIA_RUNNER", "HARNESS_PSK", "HARNESS_PSK_FILE", "HARNESS_OPERATOR_PSK", "HARNESS_OPERATOR_PSK_FILE", "HARNESS_SERVER_CID", "HARNESS_WS_PATH", cli.ServerPubKeyEnv, cli.ServerPubKeyFileEnv} {
		t.Setenv(key, "")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	s := New(Config{Addr: addr, OperatorPSK: []byte("pubsub-authz-operator")})
	s.SetBoard(newTestBoard(t))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	for {
		probe, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if err == nil {
			_ = probe.Close()
			break
		}
		if ctx.Err() != nil {
			t.Fatal("server did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	victim := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	return ctx, &pubsubAuthzServer{s: s, cid: objproto.MustParseConnectionID("ws:" + addr + "-1"), victim: victim}
}

// dialOperator connects with the operator secret.
func (e *pubsubAuthzServer) dialOperator(ctx context.Context, t *testing.T) *cli.Client {
	t.Helper()
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")
	t.Setenv("HARNESS_OPERATOR_PSK", "pubsub-authz-operator")
	c, err := cli.Dial(ctx, e.cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// dialAgent connects as the agent of a fresh task holding g.
func (e *pubsubAuthzServer) dialAgent(ctx context.Context, t *testing.T, label string, g authority.Grant) *cli.Client {
	t.Helper()
	c, _ := e.dialAgentTask(ctx, t, label, g)
	return c
}

// dialAgentTask is dialAgent that also returns the agent's task id.
func (e *pubsubAuthzServer) dialAgentTask(ctx context.Context, t *testing.T, label string, g authority.Grant) (*cli.Client, string) {
	t.Helper()
	identity := testRunnerID(label)
	id := e.s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, g, "")
	ticket := [16]byte{7}
	boardRegisterTask(e.s.Board, identity, id, ticket, "", agentboard.NewConversation)
	t.Setenv("HARNESS_OPERATOR_PSK", "")
	t.Setenv("HARNESS_RUNNER_ID", identity.Hex())
	t.Setenv("HARNESS_TASK_ID", id)
	t.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(ticket[:]))
	c, err := cli.Dial(ctx, e.cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := cli.WhoAmIWith(ctx, c); err != nil {
		t.Fatalf("agent whoami: %v", err)
	}
	return c, id
}

// dialRunner connects with the runner role, the way any connect-PSK holder can.
func (e *pubsubAuthzServer) dialRunner(ctx context.Context, t *testing.T, label string) (*peer.Conn, protocol.RunnerID) {
	t.Helper()
	pc, err := cli.DialPeerConn(ctx, e.cid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	authed := make(chan protocol.PskAuthStatus, 1)
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind == appwire.AppKind_PskAuth {
			var resp protocol.PskAuthResponse
			if err := resp.DecodeExact(payload); err == nil {
				authed <- resp.Status
			}
		}
	})
	pc.Start(ctx)
	identity := testRunnerID(label)
	// No roots: the scheduler must not hand this runner the queued victim.
	hello := protocol.RunnerHello{Version: 1, RunnerId: identity, MaxTasks: 1}
	req := buildRunnerHelloReq(nil, pc.Connection().GetTranscript(), hello)
	if _, _, err := pc.Connection().SendMessage(buildPskAuthData(t, req)); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-authed:
		if status != protocol.PskAuthStatus_Ok {
			t.Fatalf("runner handshake: %v", status)
		}
	case <-ctx.Done():
		t.Fatal("runner handshake timed out")
	}
	// The hello is dispatched after the handshake answer; wait for the entry.
	for {
		if _, ok := e.s.registry.GetByIdentity(identity); ok {
			return pc, identity
		}
		if ctx.Err() != nil {
			t.Fatal("runner never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantForbidden(t *testing.T, who, topic string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s joined %s, want the JOIN refused", who, topic)
		return
	}
	if !strings.Contains(err.Error(), pubsubproto.Status_Forbidden.String()) {
		t.Errorf("%s JOIN %s: %v, want a forbidden refusal", who, topic, err)
	}
}

// receives reports whether want arrives on st within d.
func receives(ctx context.Context, st trsf.BidirectionalStream, want []byte, d time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	got := make(chan bool, 1)
	go func() {
		fr := pubsub.NewFrameReader(st)
		for {
			p, err := fr.Next()
			if err != nil {
				got <- false
				return
			}
			if bytes.Contains(p, want) {
				got <- true
				return
			}
		}
	}()
	select {
	case ok := <-got:
		return ok
	case <-ctx.Done():
		return false
	}
}

// P1: a task log is read through `logs`' gate or not at all.
func TestPubsubAuthz_TaskLogJoinNeedsExecView(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	logTopic := topics.TaskLog(e.victim)

	none := e.dialAgent(ctx, t, "agent-none", authority.Grant{})
	_, err := none.Peer().JoinAndGetStream(ctx, "test", logTopic)
	wantForbidden(t, "a --caps none agent", logTopic, err)

	viewer := e.dialAgent(ctx, t, "agent-viewer", authority.Grant{
		Caps:  protocol.Capability_ExecView,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{e.victim}},
	})
	if _, err := viewer.Peer().JoinAndGetStream(ctx, "test", logTopic); err != nil {
		t.Errorf("an agent holding exec_view over the task: JOIN %s: %v", logTopic, err)
	}

	op := e.dialOperator(ctx, t)
	if _, err := op.Peer().JoinAndGetStream(ctx, "test", logTopic); err != nil {
		t.Errorf("operator JOIN %s: %v", logTopic, err)
	}
}

// P2, P3 and the runner hole: a connection that authenticated as a runner has
// no principal, which every visibility helper reads as "operator". Its JOINs
// are limited to the logs of tasks it owns.
func TestPubsubAuthz_RunnerRoleJoinsOnlyOwnedLogs(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	pc, identity := e.dialRunner(ctx, t, "impostor")

	for _, topic := range []string{topics.TaskLog(e.victim), topics.Notifications(), topics.RunnersStatus(), topics.TasksStatus()} {
		_, err := pc.JoinAndGetStream(ctx, "runner", topic)
		wantForbidden(t, "a runner that owns nothing", topic, err)
	}

	entry, _ := e.s.registry.GetByIdentity(identity)
	if !e.s.registry.BindTask(entry.ID, e.victim) {
		t.Fatal("BindTask")
	}
	op := e.dialOperator(ctx, t)
	opLog, err := op.Peer().JoinAndGetStream(ctx, "test", topics.TaskLog(e.victim))
	if err != nil {
		t.Fatalf("operator JOIN log: %v", err)
	}
	line := []byte("owned-runner-line\n")
	if err := pc.Publish(ctx, "runner", topics.TaskLog(e.victim), line); err != nil {
		t.Fatalf("owning runner publish: %v", err)
	}
	if !receives(ctx, opLog, line, 2*time.Second) {
		t.Error("the owning runner's log line did not reach the operator")
	}
}

// P2, P3: runner topology and notifications are for whom `ls` shows runners.
func TestPubsubAuthz_RunnersAndNotificationsAreForAllVisibility(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	agent := e.dialAgent(ctx, t, "agent-none", authority.Grant{})
	_, err := agent.Peer().JoinAndGetStream(ctx, "test", topics.Notifications())
	wantForbidden(t, "a --caps none agent", topics.Notifications(), err)

	agentRunners, err := agent.Peer().JoinAndGetStream(ctx, "test", topics.RunnersStatus())
	if err != nil {
		t.Fatalf("a confined agent's JOIN of runners.status must stay accepted (watch joins it): %v", err)
	}
	op := e.dialOperator(ctx, t)
	opRunners, err := op.Peer().JoinAndGetStream(ctx, "test", topics.RunnersStatus())
	if err != nil {
		t.Fatalf("operator JOIN runners.status: %v", err)
	}
	_, identity := e.dialRunner(ctx, t, "fresh-runner")
	want := identity.Id[:]
	if !receives(ctx, opRunners, want, 2*time.Second) {
		t.Error("operator did not hear the runner register")
	}
	if receives(ctx, agentRunners, want, 300*time.Millisecond) {
		t.Error("a confined agent heard a runner register; its ls shows no runners")
	}
}

// P4: nothing a client writes on a joined stream is published.
func TestPubsubAuthz_ClientWritesAreDropped(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	op := e.dialOperator(ctx, t)
	agent := e.dialAgent(ctx, t, "agent-viewer", authority.Grant{
		Caps:  protocol.Capability_ExecView,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{e.victim}},
	})
	for _, topic := range []string{topics.TasksStatus(), topics.TaskLog(e.victim)} {
		opSt, err := op.Peer().JoinAndGetStream(ctx, "test", topic)
		if err != nil {
			t.Fatalf("operator JOIN %s: %v", topic, err)
		}
		forged := []byte("forged-by-agent:" + topic)
		if err := agent.Peer().Publish(ctx, "agent", topic, forged); err != nil {
			t.Fatalf("agent publish %s: %v", topic, err)
		}
		if receives(ctx, opSt, forged, 500*time.Millisecond) {
			t.Errorf("an agent's write on %s reached the operator", topic)
		}
	}
}

// D6: read permission is asked again on every publish, so narrowing a reader's
// grant stops the next chunk without waiting for it to leave the topic.
func TestPubsubAuthz_NarrowingStopsTheNextLogChunk(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	pc, identity := e.dialRunner(ctx, t, "owner")
	entry, _ := e.s.registry.GetByIdentity(identity)
	if !e.s.registry.BindTask(entry.ID, e.victim) {
		t.Fatal("BindTask")
	}
	viewer, viewerID := e.dialAgentTask(ctx, t, "agent-viewer", authority.Grant{
		Caps:  protocol.Capability_ExecView,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{e.victim}},
	})
	logTopic := topics.TaskLog(e.victim)
	st, err := viewer.Peer().JoinAndGetStream(ctx, "test", logTopic)
	if err != nil {
		t.Fatalf("viewer JOIN: %v", err)
	}
	before := []byte("before-narrowing\n")
	if err := pc.Publish(ctx, "runner", logTopic, before); err != nil {
		t.Fatal(err)
	}
	if !receives(ctx, st, before, 2*time.Second) {
		t.Fatal("the viewer did not get the chunk it was entitled to")
	}
	// The store, not the set_caps verb: the verb also drops the narrowed task's
	// connections, which would pass this test without the per-publish check.
	e.s.tasks.SetCaps(viewerID, GrantUpdate{Caps: true, To: authority.Grant{Caps: protocol.Capability_None}}, AuthorityWrite{})
	after := []byte("after-narrowing\n")
	if err := pc.Publish(ctx, "runner", logTopic, after); err != nil {
		t.Fatal(err)
	}
	if receives(ctx, st, after, 500*time.Millisecond) {
		t.Error("a reader whose exec_view was withdrawn still got the next chunk")
	}
}
