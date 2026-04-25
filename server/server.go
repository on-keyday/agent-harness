package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
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

	// publishTaskEvent constructs and publishes a TaskStatusEvent to the
	// global tasks.status topic and the per-task task.<id>.status topic.
	publishTaskEvent := func(taskID string, kind protocol.StatusEventKind, status protocol.TaskStatus, exitCode int32) {
		ev := protocol.TaskStatusEvent{
			Kind:       kind,
			Ts:         uint64(time.Now().UnixNano()),
			TaskStatus: status,
			ExitCode:   exitCode,
		}
		raw, err := hex.DecodeString(taskID)
		if err == nil {
			copy(ev.TaskId.Id[:], raw)
		}
		payload := ev.MustAppend(nil)
		s.pubsub.Publish("server", topics.TasksStatus(), payload)
		s.pubsub.Publish("server", topics.TaskStatus(taskID), payload)
	}

	// publishRunnerEvent constructs and publishes a RunnerStatusEvent to
	// the global runners.status topic.
	publishRunnerEvent := func(_ string, kind protocol.StatusEventKind, status protocol.RunnerStatus) {
		ev := protocol.RunnerStatusEvent{
			Kind:         kind,
			Ts:           uint64(time.Now().UnixNano()),
			RunnerStatus: status,
			RunnerId:     placeholderRunnerID(),
		}
		payload := ev.MustAppend(nil)
		s.pubsub.Publish("server", topics.RunnersStatus(), payload)
	}

	// Wire task lifecycle hooks.
	// OnCreate publishes task_queued; Run may wrap this further for log store taps.
	s.tasks.OnCreate = func(id string) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskQueued, protocol.TaskStatus_Queued, 0)
	}
	s.tasks.OnAssign = func(id, runnerID, worktreeDir string) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskAssigned, protocol.TaskStatus_Running, 0)
	}
	s.tasks.OnFinish = func(id string, exit int32, status protocol.TaskStatus) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskEnded, status, exit)
	}
	s.tasks.OnCancel = func(id string) {
		publishTaskEvent(id, protocol.StatusEventKind_TaskEnded, protocol.TaskStatus_Cancelled, 0)
	}

	// Wire registry hooks.
	s.registry.OnAdd = func(entry RunnerEntry) {
		publishRunnerEvent(entry.ID, protocol.StatusEventKind_RunnerRegistered, protocol.RunnerStatus_Idle)
	}
	s.registry.OnRemove = func(id string) {
		publishRunnerEvent(id, protocol.StatusEventKind_RunnerOffline, protocol.RunnerStatus_Offline)
	}

	// Wire TaskStarted hook so the runner_handler can publish the event.
	s.runnerHandler.OnTaskStarted = func(taskID string) {
		publishTaskEvent(taskID, protocol.StatusEventKind_TaskStarted, protocol.TaskStatus_Running, 0)
	}

	return s
}

// Run listens on cfg.Addr until ctx is done. Returns the first fatal error.
//
// If cfg.DataDir is non-empty, Run creates the directory, replays any existing
// WAL into the TaskStore (rebuilding Queued tasks and marking interrupted
// Running tasks as Failed), then opens the WAL for new appends.
// If cfg.DataDir is empty, WAL setup is skipped entirely (safe for tests that
// do not need persistence).
func (s *Server) Run(ctx context.Context) error {
	if s.cfg.DataDir != "" {
		if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}
		walPath := filepath.Join(s.cfg.DataDir, "events.log")
		// Replay WAL if present. A corrupted WAL is logged but does not prevent
		// server startup — an empty store is recoverable.
		events, rerr := ReadWAL(walPath)
		if rerr != nil {
			s.cfg.Logger.Error("WAL replay failed", "path", walPath, "err", rerr)
		} else if events != nil {
			s.tasks.ReplayEvents(events)
		}
		wal, err := OpenWAL(walPath)
		if err != nil {
			return fmt.Errorf("open WAL: %w", err)
		}
		defer wal.Close()
		s.tasks.SetWAL(wal)

		logStore, err := NewLogStore(filepath.Join(s.cfg.DataDir, "logs"))
		if err != nil {
			return fmt.Errorf("open log store: %w", err)
		}
		defer logStore.Close()

		// Chain log store tap into the existing OnCreate hook (which publishes task_queued).
		existingOnCreate := s.tasks.OnCreate
		s.tasks.OnCreate = func(taskID string) {
			if existingOnCreate != nil {
				existingOnCreate(taskID)
			}
			// Register log store tap for this task.
			topic := topics.TaskLog(taskID)
			s.pubsub.TapSubscribe(topic, func(_ string, msg []byte) {
				if err := logStore.Append(taskID, msg); err != nil {
					s.cfg.Logger.Error("logstore append", "task", taskID, "err", err)
				}
			})
		}

		// Register taps for tasks that survived replay and may still emit logs.
		for _, t := range s.tasks.List(0) {
			if t.Status == protocol.TaskStatus_Queued || t.Status == protocol.TaskStatus_Running {
				taskID := t.ID
				topic := topics.TaskLog(taskID)
				s.pubsub.TapSubscribe(topic, func(_ string, msg []byte) {
					logStore.Append(taskID, msg) //nolint:errcheck
				})
			}
		}
	}
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
