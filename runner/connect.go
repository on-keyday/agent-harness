package runner

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Config holds the configuration for the runner connection.
type Config struct {
	ServerAddr      string   // host:port
	RepoPath        string   // absolute path of the repo this runner serves
	ClaudeBin       string   // path to the claude binary
	ExtraClaudeArgs []string // forwarded to every claude invocation (before -p)
	Logger          *slog.Logger
}

// Run connects to the server, registers via Hello, and processes AssignTask requests until ctx is done.
// Returns the first fatal error.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	sess, err := transport.WebSocketSession(cfg.Logger, cfg.ServerAddr, nil, objproto.SessionModeClient)
	if err != nil {
		return fmt.Errorf("ws session: %w", err)
	}

	// Generate a connection ID in the same format as harness-client: "ws:127.0.0.1:<port>-1111"
	// The format is: "<transport>:<addr>-<id>" where addr is a netip.AddrPort string.
	cid := objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%s-1111", portFrom(cfg.ServerAddr)))
	conn, err := objproto.DoECDHHandshake(ctx, sess, cid, ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return fmt.Errorf("ecdh handshake: %w", err)
	}
	// On exit, tell the server we're going away so the registry deregisters
	// us immediately instead of waiting for the AutoGarbageCollect timeout.
	defer trsf.SendClose(conn) //nolint:errcheck

	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, cfg.Logger)
	go trsf.AutoSend(ctx, p, conn, nil)
	// Keep the objproto session alive while the runner is idle (no incoming AssignTask).
	// Server's AutoGarbageCollect drops sessions after 1 minute of silence.
	go trsf.AutoPing(ctx, conn, 30*time.Second)

	// Build the production Sender.
	sender := newConnSender(ctx, conn, p, cfg.Logger)

	// Send Hello.
	hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	h := protocol.RunnerHello{Version: 1}
	h.SetRepoPath([]byte(cfg.RepoPath))
	hello.SetHello(h)
	helloBytes := hello.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err := sender.Send(helloBytes); err != nil {
		return fmt.Errorf("send Hello: %w", err)
	}

	session := &Session{
		RepoPath:        cfg.RepoPath,
		ClaudeBin:       cfg.ClaudeBin,
		ExtraClaudeArgs: cfg.ExtraClaudeArgs,
		Sender:          sender,
		Now:             time.Now,
	}

	// Receive loop. trsf.AutoReceive blocks until ctx is done, the connection
	// breaks, or the server sends a Close (in which case AutoReceive returns
	// after dispatching a (nil, io.EOF) event).
	trsf.AutoReceive(ctx, p, conn, func(msg *objproto.Message, err error) {
		if err != nil {
			return
		}
		if msg == nil || len(msg.Data) == 0 {
			return
		}
		kind := wire.ApplicationPayloadKind(msg.Data[0])
		if kind != wire.ApplicationPayloadKind_RunnerControl {
			return // ignore other kinds for the runner side
		}
		req := &protocol.RunnerRequest{}
		if _, derr := req.Decode(msg.Data[1:]); derr != nil {
			cfg.Logger.Error("runner_request decode", "err", derr)
			return
		}
		switch req.Kind {
		case protocol.RunnerRequestType_AssignTask:
			at := req.AssignTask()
			if at == nil {
				return
			}
			// Run in a separate goroutine so the receive loop is not blocked by a long task.
			go session.handleAssign(ctx, at)
		case protocol.RunnerRequestType_CancelTask:
			// v1 does not implement runner-side cancel; log and ignore.
			cfg.Logger.Info("runner: cancel not implemented", "kind", req.Kind)
		}
	})
	return nil
}

// portFrom extracts the port portion from a "host:port" string.
// Falls back to the full string if no colon is found.
func portFrom(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}

// connSender is the production implementation of Sender that writes to a real
// objproto.Connection and trsf.Transport.
type connSender struct {
	conn   objproto.Connection
	p      trsf.Transport
	logger *slog.Logger

	mu      sync.Mutex
	streams map[string]trsf.BidirectionalStream // topic → stream (cached after first join)
	pending map[string]chan trsf.BidirectionalStream
}

func newConnSender(ctx context.Context, conn objproto.Connection, p trsf.Transport, logger *slog.Logger) *connSender {
	cs := &connSender{
		conn:    conn,
		p:       p,
		logger:  logger,
		streams: make(map[string]trsf.BidirectionalStream),
		pending: make(map[string]chan trsf.BidirectionalStream),
	}
	go cs.acceptLoop(ctx)
	return cs
}

// acceptLoop continuously accepts bidirectional streams from the server.
// Each stream begins with a "<topic>\n" header that the server writes upon subscribing.
// Once decoded, the stream is delivered to the goroutine waiting in Publish.
func (c *connSender) acceptLoop(ctx context.Context) {
	for {
		st, err := c.p.AcceptBidirectionalStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Error("runner: acceptBidirectionalStream error", "err", err)
			return
		}
		// The server prepends the topic name followed by a newline.
		// Read enough bytes to identify the header (up to 64 KB).
		data, _, err := st.ReadDirect(trsf.InitialFlowWindow)
		if err != nil {
			c.logger.Error("runner: reading topic header from stream", "err", err)
			continue
		}
		// Extract topic: everything before the first '\n'.
		newlineIdx := bytes.IndexByte(data, '\n')
		if newlineIdx < 0 {
			c.logger.Error("runner: topic header has no newline", "data", string(data))
			continue
		}
		topic := string(data[:newlineIdx])

		c.mu.Lock()
		ch, ok := c.pending[topic]
		if ok {
			delete(c.pending, topic)
		}
		c.mu.Unlock()

		if !ok {
			c.logger.Warn("runner: received unexpected stream for topic", "topic", topic)
			continue
		}
		ch <- st
	}
}

// Send transmits a control-frame message. objproto.Connection.SendMessage is thread-safe.
func (c *connSender) Send(data []byte) error {
	_, _, err := c.conn.SendMessage(data)
	return err
}

// ID returns the runner's connection ID.
func (c *connSender) ID() objproto.ConnectionID { return c.conn.ConnectionID() }

// Publish writes data to a per-topic bidirectional stream. The stream is created lazily
// on first Publish to a given topic. Thread-safe.
func (c *connSender) Publish(topic string, data []byte) error {
	c.mu.Lock()
	st, ok := c.streams[topic]
	if ok {
		c.mu.Unlock()
		return st.AppendData(false, data)
	}

	// Check if there is already a pending join in progress.
	ch, pending := c.pending[topic]
	if !pending {
		// Issue a JOIN so the server creates the stream. The runner relies on
		// AcceptBidirectionalStream + topic-header dispatch in acceptLoop and
		// discards the PubSubResponse (AutoReceive's ignore-non-RunnerControl
		// path), so reqID is unused here.
		joinBytes := pubsub.JoinTopic(0, "runner", topic)
		if _, _, err := c.conn.SendMessage(joinBytes); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("join topic %q: %w", topic, err)
		}
		ch = make(chan trsf.BidirectionalStream, 1)
		c.pending[topic] = ch
	}
	c.mu.Unlock()

	// Wait for the acceptLoop to deliver the server-created stream.
	st = <-ch

	c.mu.Lock()
	c.streams[topic] = st
	c.mu.Unlock()

	return st.AppendData(false, data)
}
