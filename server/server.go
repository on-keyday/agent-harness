package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Config holds the configuration for a Server instance.
type Config struct {
	Addr    string // host:port for the WebSocket listener
	DataDir string // reserved for WAL/log persistence (Tasks 2.8 / 2.9 / 2.9b)
	Logger  *slog.Logger
}

// Server wires all components together and manages the main accept loop.
type Server struct {
	cfg           Config
	registry      *Registry
	tasks         *TaskStore
	pubsub        *pubsub.PubSub
	scheduler     *Scheduler
	runnerHandler *RunnerHandler
	taskHandler   *TaskHandler
	dispatcher    *Dispatcher
}

// New constructs a Server with all components wired but NOT yet listening.
// Callers in tests can inspect / inject pieces after construction.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		registry: NewRegistry(),
		tasks:    NewTaskStore(),
		pubsub:   pubsub.NewPubSub(cfg.Logger),
	}
	s.scheduler = NewScheduler(s.registry, s.tasks, s.sendAssign)
	s.runnerHandler = &RunnerHandler{
		Registry: s.registry,
		Tasks:    s.tasks,
		Now:      time.Now,
		OnChange: s.scheduler.Tick,
	}
	s.taskHandler = &TaskHandler{
		Tasks:    s.tasks,
		Registry: s.registry,
		OnChange: s.scheduler.Tick,
	}
	s.dispatcher = &Dispatcher{
		OnRunnerControl: s.runnerHandler.Handle,
		OnTaskControl:   s.taskHandler.Handle,
	}
	return s
}

// Run listens on cfg.Addr until ctx is done. Returns the first fatal error.
func (s *Server) Run(ctx context.Context) error {
	sess, err := transport.WebSocketSession(s.cfg.Logger, s.cfg.Addr, nil, objproto.SessionModeServer)
	if err != nil {
		return fmt.Errorf("websocket session: %w", err)
	}
	go objproto.AutoGarbageCollect(sess, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
	ch := sess.GetNewActiveSessionChannel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case session, ok := <-ch:
			if !ok {
				return nil
			}
			go s.handleConnection(ctx, session)
		}
	}
}

// handleConnection manages a single active objproto connection for its lifetime.
func (s *Server) handleConnection(ctx context.Context, session objproto.Connection) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := trsf.NewStreams(connCtx, true, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, session, s.cfg.Logger)
	subscriber := pubsub.NewSubscriber(session.ConnectionID(), p)
	defer subscriber.LeaveAll(s.pubsub)

	go trsf.AutoSend(connCtx, p, session, nil)

	trsf.AutoReceive(connCtx, p, session, func(msg *objproto.Message, err error) {
		if err != nil || len(msg.Data) == 0 {
			return
		}
		kind := wire.ApplicationPayloadKind(msg.Data[0])
		if kind == wire.ApplicationPayloadKind_Pubsub {
			if resp := subscriber.HandleMessage(s.pubsub, msg.Data[1:]); resp != nil {
				session.SendMessage(resp) //nolint:errcheck
			}
			return
		}
		s.dispatcher.Dispatch(session, msg.Data)
	})

	// Connection closed: deregister the runner if present and trigger rescheduling.
	s.registry.Remove(session.ConnectionID().String())
	s.scheduler.Tick()
}

// sendAssign sends an AssignTask runner-control message to the runner identified by runnerID.
// It is used as the AssignFunc supplied to the Scheduler.
func (s *Server) sendAssign(runnerID, taskID string) error {
	entry, ok := s.registry.Get(runnerID)
	if !ok || entry.Conn == nil {
		return fmt.Errorf("runner %s not connected", runnerID)
	}
	task, ok := s.tasks.Get(taskID)
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskID)
	copy(tid.Id[:], raw)
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_AssignTask}
	req.SetAssignTask(protocol.AssignTask{TaskId: tid, Prompt: []byte(task.Prompt)})
	data := req.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	_, _, err := entry.Conn.SendMessage(data)
	return err
}
