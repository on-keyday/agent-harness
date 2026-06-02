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
)

// handleOpenPortForward waits for the relayed stream, dials the requested
// TCP target from the runner host, and splices the two. On dial failure it
// closes the stream (the server splice propagates EOF to the client, which
// closes the accepted local connection — connection-refused semantics).
func (s *Session) handleOpenPortForward(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
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
