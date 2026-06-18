package agentboard_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/objproto"
)

func trsfStreamIDForTest(id uint64) trsf.StreamID { return trsf.StreamID(id) }

// freePort finds an available TCP port on loopback and returns the full addr
// "127.0.0.1:<port>".  The socket is closed before returning — there is a
// small TOCTOU window, but it is acceptable for in-process test use.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// mkRid builds a protocol.RunnerID for registry registration.
func mkRid(n uint16) protocol.RunnerID {
	var r protocol.RunnerID
	r.SetTransport([]byte("ws"))
	r.SetIpAddr([]byte{127, 0, 0, 1})
	r.Port = 9000
	r.UniqueNumber = n
	return r
}

// mkTid builds a protocol.TaskID for registry registration.
func mkTid(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}


// startServer constructs a server.Server with a Board, binds it to addr,
// and starts it in a goroutine.  Returns (board, cancel) — cancel stops the
// server.
func startServer(t *testing.T, addr string) (*agentboard.Board, context.CancelFunc) {
	t.Helper()

	board := agentboard.New(agentboard.Config{
		RingN:      64,
		TopicTTL:   time.Hour,
		MaxTopics:  32,
		MaxPayload: 4096,
	})

	s := server.New(server.Config{Addr: addr})
	s.SetBoard(board)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()

	// Poll until the HTTP server is ready to accept connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		board.Close()
	})

	return board, cancel
}

// dialAgent dials the server, completes the ECDH/trsf/peer handshake, sends
// a ClientHello (AppKind_TaskControl), waits for the ClientHelloResponse, and
// returns (conn, status).
//
// All AgentMessage payloads received after the Hello are forwarded to msgCh
// (may be nil to discard them).
func dialAgent(
	t *testing.T,
	ctx context.Context,
	serverAddr string,
	rid protocol.RunnerID,
	tid protocol.TaskID,
	ticket [16]byte,
	msgCh chan *agentboard.AgentMessage,
) (pc *peer.Conn, helloStatus protocol.ClientHelloStatus) {
	t.Helper()

	peerCID, err := objproto.ParseConnectionID(
		fmt.Sprintf("ws:%s-*", serverAddr),
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr,
	)
	if err != nil {
		t.Fatalf("dialAgent: parse cid: %v", err)
	}

	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   cli.WebSocketPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		t.Fatalf("dialAgent: ws endpoint: %v", err)
	}

	pc, err = peer.Dial(ctx, ep, peerCID, peer.DialConfig{})
	if err != nil {
		t.Fatalf("dialAgent: dial: %v", err)
	}

	// Receive channel for ClientHelloResponse — buffered so the goroutine never blocks.
	helloCh := make(chan protocol.ClientHelloStatus, 1)
	var helloOnce sync.Once

	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		switch kind {
		case appwire.AppKind_TaskControl:
			var resp protocol.TaskControlResponse
			if _, err := resp.Decode(payload); err != nil {
				return
			}
			if resp.Kind == protocol.TaskControlKind_ClientHello {
				if r := resp.ClientHello(); r != nil {
					helloOnce.Do(func() { helloCh <- r.Status })
				}
			}
		case appwire.AppKind_AgentMessage:
			msg := &agentboard.AgentMessage{}
			if _, err := msg.Decode(payload); err != nil {
				return
			}
			// Forward all AgentMessages to the caller's channel.
			if msgCh != nil {
				select {
				case msgCh <- msg:
				default:
				}
			}
		}
	})

	pc.Start(ctx)

	// Build and send ClientHello via AppKind_TaskControl.
	info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
	hello := protocol.ClientHello{Kind: protocol.ClientKind_Agent}
	hello.SetAgentInfo(info)
	req := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
	req.SetClientHello(hello)
	helloBytes, err := req.Append([]byte{byte(appwire.AppKind_TaskControl)})
	if err != nil {
		t.Fatalf("dialAgent: encode hello: %v", err)
	}
	if _, _, err := pc.Connection().SendMessage(helloBytes); err != nil {
		t.Fatalf("dialAgent: send hello: %v", err)
	}

	// Wait for ClientHelloResponse.
	select {
	case status := <-helloCh:
		return pc, status
	case <-time.After(2 * time.Second):
		t.Fatal("dialAgent: timed out waiting for ClientHelloResponse")
		return nil, protocol.ClientHelloStatus_BadTicket // unreachable
	}
}

// sendAgentMsg is a helper to encode and send an AgentMessage over a peer.Conn.
func sendAgentMsg(t *testing.T, pc *peer.Conn, msg *agentboard.AgentMessage) {
	t.Helper()
	data, err := msg.Append([]byte{byte(appwire.AppKind_AgentMessage)})
	if err != nil {
		t.Fatalf("sendAgentMsg encode: %v", err)
	}
	if _, _, err := pc.Connection().SendMessage(data); err != nil {
		t.Fatalf("sendAgentMsg send: %v", err)
	}
}

// TestAgentboardE2E_HelloSendWait verifies the success path:
// two agents dial the server, Agent A subscribes + sends to "topic/foo",
// Agent B subscribes and issues a Wait — the Wait must return A's message.
func TestAgentboardE2E_HelloSendWait(t *testing.T) {
	addr := freePort(t)
	board, _ := startServer(t, addr)

	// Pre-register two tickets.
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xAA
	ticketB[0] = 0xBB

	board.Registry().Register(mkRid(1), mkTid(1), ticketA)
	board.Registry().Register(mkRid(2), mkTid(2), ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Channels to receive AgentMessages after Hello.
	msgChA := make(chan *agentboard.AgentMessage, 8)
	msgChB := make(chan *agentboard.AgentMessage, 8)

	// Dial Agent A.
	connA, statusA := dialAgent(t, ctx, addr, mkRid(1), mkTid(1), ticketA, msgChA)
	defer connA.Close()
	if statusA != protocol.ClientHelloStatus_Ok {
		t.Fatalf("Agent A Hello status = %v, want Ok", statusA)
	}

	// Dial Agent B.
	connB, statusB := dialAgent(t, ctx, addr, mkRid(2), mkTid(2), ticketB, msgChB)
	defer connB.Close()
	if statusB != protocol.ClientHelloStatus_Ok {
		t.Fatalf("Agent B Hello status = %v, want Ok", statusB)
	}

	// Agent B subscribes to "topic/foo".
	subB := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Subscribe}
	sr := agentboard.SubscribeRequest{RequestId: 1}
	sr.SetPattern([]byte("topic/foo"))
	if !subB.SetSubscribe(sr) {
		t.Fatal("SetSubscribe failed")
	}
	sendAgentMsg(t, connB, subB)

	// Wait for SubscribeResponse from B.
	select {
	case msg := <-msgChB:
		if msg.Kind != agentboard.AgentMessageKind_SubscribeResponse {
			t.Fatalf("expected SubscribeResponse, got %v", msg.Kind)
		}
		resp := msg.SubscribeResponse()
		if resp == nil || resp.Status != agentboard.SubscribeStatus_Ok {
			t.Fatalf("subscribe status = %v, want Ok", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SubscribeResponse from B")
	}

	// Agent A sends a message to "topic/foo". Payload travels on a
	// client-initiated send-stream; envelope carries only the stream id.
	payloadStream := connA.Transport().CreateSendStream()
	if payloadStream == nil {
		t.Fatal("CreateSendStream returned nil")
	}
	if err := payloadStream.AppendData(false, []byte("hello-from-A")); err != nil {
		t.Fatalf("payload write: %v", err)
	}
	if err := payloadStream.AppendData(true); err != nil {
		t.Fatalf("payload EOF: %v", err)
	}
	sendMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Send}
	req := agentboard.SendRequest{RequestId: 2, PayloadStreamId: uint64(payloadStream.ID())}
	req.SetTopic([]byte("topic/foo"))
	if !sendMsg.SetSend(req) {
		t.Fatal("SetSend failed")
	}
	sendAgentMsg(t, connA, sendMsg)

	// Wait for SendResponse from A.
	select {
	case msg := <-msgChA:
		if msg.Kind != agentboard.AgentMessageKind_SendResponse {
			t.Fatalf("expected SendResponse from A, got %v", msg.Kind)
		}
		resp := msg.SendResponse()
		if resp == nil || resp.Status != agentboard.SendStatus_Ok {
			t.Fatalf("send status = %v, want Ok", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SendResponse from A")
	}

	// Agent B issues a Wait on "topic/foo".
	waitMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Wait}
	wr := agentboard.WaitRequest{RequestId: 3, Since: 0, TimeoutMs: 2000}
	wr.SetPattern([]byte("topic/foo"))
	if !waitMsg.SetWait(wr) {
		t.Fatal("SetWait failed")
	}
	sendAgentMsg(t, connB, waitMsg)

	// Wait for WaitResponse from B.
	select {
	case msg := <-msgChB:
		if msg.Kind != agentboard.AgentMessageKind_WaitResponse {
			t.Fatalf("expected WaitResponse from B, got %v", msg.Kind)
		}
		resp := msg.WaitResponse()
		if resp == nil {
			t.Fatal("WaitResponse is nil")
		}
		if resp.TimedOut != 0 {
			t.Fatal("WaitResponse timed out unexpectedly")
		}
		if len(resp.Msgs) == 0 {
			t.Fatal("WaitResponse has no messages")
		}
		// Read payload from the server-allocated send-stream.
		streamID := resp.Msgs[0].PayloadStreamId
		if streamID == 0 {
			t.Fatal("DeliveredMessage has no PayloadStreamId")
		}
		var rs interface {
			ReadDirect(uint64) ([]byte, bool, error)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if r := connB.Transport().GetReceiveStream(trsfStreamIDForTest(streamID)); r != nil {
				rs = r
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if rs == nil {
			t.Fatalf("payload stream %d not visible after 2s", streamID)
		}
		var got []byte
		for {
			data, eof, err := rs.ReadDirect(64 * 1024)
			if err != nil {
				t.Fatalf("payload read: %v", err)
			}
			if len(data) > 0 {
				got = append(got, data...)
			}
			if eof {
				break
			}
		}
		if string(got) != "hello-from-A" {
			t.Fatalf("WaitResponse payload = %q, want %q", got, "hello-from-A")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for WaitResponse from B")
	}
}

// TestAgentboardE2E_BadTicketRejected verifies that connecting with a wrong
// ticket results in HelloStatus_BadTicket.
func TestAgentboardE2E_BadTicketRejected(t *testing.T) {
	addr := freePort(t)
	board, _ := startServer(t, addr)

	// Register a valid ticket.
	var validTicket [16]byte
	validTicket[0] = 0xCC
	board.Registry().Register(mkRid(3), mkTid(3), validTicket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Dial with a WRONG ticket.
	var wrongTicket [16]byte
	wrongTicket[0] = 0xFF

	conn, status := dialAgent(t, ctx, addr, mkRid(3), mkTid(3), wrongTicket, nil)
	defer conn.Close()

	if status != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("expected ClientHelloStatus_BadTicket, got %v", status)
	}
}
