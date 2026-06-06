package runner

import (
	"context"
	"encoding/hex"
	"net"
	"strconv"
	"sync"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// bidiStreamCreator makes a bidi stream toward the server. Satisfied by
// trsf.Transport (= pc.Transport()); the remote port-forward path creates one
// stream per accepted connection.
type bidiStreamCreator interface {
	CreateBidirectionalStream() trsf.BidirectionalStream
}

// remoteForwardListeners tracks remote port-forward TCP listeners by forwardId.
type remoteForwardListeners struct {
	mu sync.Mutex
	m  map[uint64]net.Listener
}

func (r *remoteForwardListeners) add(id uint64, ln net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[uint64]net.Listener{}
	}
	r.m[id] = ln
}

func (r *remoteForwardListeners) close(id uint64) {
	r.mu.Lock()
	ln := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// rforwardListeners lazily returns the session's listener registry.
func (s *Session) rforwardListeners() *remoteForwardListeners {
	if s.rforwards == nil {
		s.rforwards = &remoteForwardListeners{}
	}
	return s.rforwards
}

// handleOpenPortForward waits for the relayed stream, dials the requested
// TCP target from the runner host, and splices the two. On dial failure it
// closes the stream (the server splice propagates EOF to the client, which
// closes the accepted local connection — connection-refused semantics).
func (s *Session) handleOpenPortForward(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
	if req.Direction == protocol.PortForwardDirection_Remote {
		s.startRemoteForward(ctx, req)
		return
	}
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("port_forward: stream not visible", "stream_id", req.StreamId)
		return
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	if s.worktreeDirFor(taskIDHex) == "" {
		log.Error("port_forward: unknown task", "task_id", taskIDHex)
		_ = stream.CloseBoth()
		return
	}
	addr := net.JoinHostPort(string(req.RemoteHost), strconv.Itoa(int(req.RemotePort)))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Info("port_forward: dial failed", "addr", addr, "err", err)
		_ = stream.CloseBoth()
		return
	}
	spliceConnStream(conn, stream)
}

// startRemoteForward (ssh -R) opens a listener for a remote-forward
// registration. Each accepted connection becomes a new stream to the server,
// announced via RunnerMessage{RemoteForwardConn}; the server then drives the
// client to dial the real target.
func (s *Session) startRemoteForward(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
	log := s.logger()
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	if s.worktreeDirFor(taskIDHex) == "" {
		log.Error("remote_forward: unknown task", "task_id", taskIDHex)
		return
	}
	if s.creator == nil {
		log.Error("remote_forward: no stream creator wired")
		return
	}
	ln, err := s.startRemoteForwardListener(ctx, req.ForwardId, string(req.BindAddr), int(req.BindPort))
	if err != nil {
		// The server already returned Ok at registration time; a listen failure
		// here just means no connections will ever arrive. Log it. (A precise
		// BindFailed reply would need a runner→server ack; out of scope.)
		log.Info("remote_forward: listen failed", "addr", net.JoinHostPort(string(req.BindAddr), strconv.Itoa(int(req.BindPort))), "err", err)
		return
	}
	s.rforwardListeners().add(req.ForwardId, ln)
	log.Info("remote_forward: listening", "forward_id", req.ForwardId, "addr", ln.Addr().String())
}

// startRemoteForwardListener binds a TCP listener and starts an accept loop that
// routes each connection through onRemoteForwardConn.
func (s *Session) startRemoteForwardListener(ctx context.Context, forwardID uint64, bindAddr string, bindPort int) (net.Listener, error) {
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, strconv.Itoa(bindPort)))
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go s.onRemoteForwardConn(forwardID, conn)
		}
	}()
	return ln, nil
}

// onRemoteForwardConn creates a stream for one accepted connection, tells the
// server about it, and splices the connection to the stream.
func (s *Session) onRemoteForwardConn(forwardID uint64, conn net.Conn) {
	stream := s.creator.CreateBidirectionalStream()
	if stream == nil {
		_ = conn.Close()
		return
	}
	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_RemoteForwardConn}
	msg.SetRemoteForwardConn(protocol.RemoteForwardConn{ForwardId: forwardID, StreamId: uint64(stream.ID())})
	if err := s.Sender.Send(msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})); err != nil {
		_ = stream.CloseBoth()
		_ = conn.Close()
		return
	}
	spliceConnStream(conn, stream)
}

// spliceConnStream pumps bytes between a net.Conn and a trsf bidi stream
// until either direction closes or errors, then tears down both. Mirrors
// cli.spliceConnStream (kept per-package; the file-transfer handlers follow
// the same no-cross-package-sharing convention).
func spliceConnStream(conn net.Conn, st trsf.BidirectionalStream) {
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = st.CloseBoth()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // conn -> stream
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				// AppendData stores the slice by reference and copies it
				// asynchronously (trsf/send_stream.go: "data must be copied
				// before calling AppendData"). buf is reused next iteration.
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if werr := st.AppendData(false, chunk); werr != nil {
					return
				}
			}
			if err != nil {
				_ = st.AppendData(true)
				return
			}
		}
	}()
	go func() { // stream -> conn
		defer wg.Done()
		defer teardown()
		for {
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return
			}
			if len(data) > 0 {
				if _, werr := conn.Write(data); werr != nil {
					return
				}
			}
			if eof {
				return
			}
		}
	}()
	wg.Wait()
}
