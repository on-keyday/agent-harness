package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// sessionStart is everything starting a session needs once a runner has been
// chosen and the task reserved on it (Assigned and bound). Both an
// interactive open (handleOpenInteractive) and Tick's placement of a queued
// background task (StartQueuedSession) start sessions through startSession,
// so the two cannot drift (pillar A spec, A-D12).
type sessionStart struct {
	TaskIDHex          string
	Runner             RunnerEntry
	Profile, Repo      string
	Args               []string
	ResumeConversation bool
	EventStream        bool
	Detached           bool
	Rows, Cols         uint16               // initial PTY size of a detached open; 0 in either = none
	X11                *protocol.X11Forward // nil = off
	TUIConn            ConnHandle           // the requesting client; nil when Detached
	Prompt             string               // the agent's first turn; "" = none (A-D3)
}

// startSession issues the ticket, opens the runner stream (and the client
// stream for an attached open), sends OpenExec — with the prompt on its own
// send stream first, as sendAssign sends AssignTask's body — and builds the
// session's mux. On any failure the task is finished with the reason, unbound
// and revoked, and a non-ok status is returned; the caller answers with it.
func (h *TaskHandler) startSession(ss sessionStart) (trsf.BidirectionalStream, protocol.OpenInteractiveStatus) {
	runner := ss.Runner
	taskIDHex := ss.TaskIDHex
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)

	finishWithError := func(reason string) {
		slog.Error("startSession: "+reason, "task", taskIDHex, "runner", runner.Hostname)
		h.Tasks.Finish(taskIDHex, -1, []byte("server: "+reason))
		h.Registry.UnbindTask(runner.ID, taskIDHex)
		boardRevokeTask(h.Board, runner.Identity, taskIDHex)
	}
	internal := protocol.OpenInteractiveStatus_InternalError

	// Generate a fresh ticket for the agent Hello handshake.
	var ticket [16]byte
	if _, err := rand.Read(ticket[:]); err != nil {
		finishWithError("ticket gen failed: " + err.Error())
		return nil, internal
	}
	boardRegisterTask(h.Board, runner.Identity, taskIDHex, ticket, ss.Profile, conversationOf(ss.ResumeConversation))

	// A detached open creates no client stream: nobody takes the seat.
	var tuiStream trsf.BidirectionalStream
	closeTui := func() {
		if tuiStream != nil {
			_ = tuiStream.CloseBoth()
		}
	}
	if !ss.Detached {
		if ss.TUIConn == nil {
			finishWithError("attached open with no client connection")
			return nil, internal
		}
		tuiStream = ss.TUIConn.CreateBidirectionalStream()
		if tuiStream == nil {
			finishWithError("create client-side stream failed")
			return nil, internal
		}
	}
	runnerConn := runner.Conn
	if runnerConn == nil {
		closeTui()
		finishWithError("runner conn nil")
		return nil, internal
	}
	runnerStream := runnerConn.CreateBidirectionalStream()
	if runnerStream == nil {
		closeTui()
		finishWithError("create runner-side stream failed")
		return nil, internal
	}
	fail := func(reason string) (trsf.BidirectionalStream, protocol.OpenInteractiveStatus) {
		closeTui()
		_ = runnerStream.CloseBoth()
		finishWithError(reason)
		return nil, internal
	}

	// Tell the runner to wire its stream end to the agent.
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenExec}
	oer := protocol.OpenExecRunnerRequest{
		TaskId:     tid,
		StreamId:   uint64(runnerStream.ID()),
		AuthTicket: ticket,
		// The args the task runs with: the request's, or the record's on an
		// exact resume or a queued start. The task record holds the same slice.
		ExtraArgs: protocol.AgentArgsFromStrings(ss.Args),
	}
	oer.SetResumeConversation(ss.ResumeConversation)
	// Relay the kind. The runner refuses it when the resolved profile has no
	// stream adapter, so the check lives where the profiles are rather than
	// being duplicated here from a stale copy of them.
	oer.SetEventStream(ss.EventStream)
	oer.SetRepoPath([]byte(ss.Repo))
	oer.SetAgentProfile([]byte(ss.Profile)) // server-resolved profile the runner execs with (empty → runner default)
	if ss.X11 != nil && ss.EventStream {
		return fail("x11 forwarding is a terminal feature and an event-stream task has no terminal")
	}
	if ss.X11 != nil && ss.Detached {
		return fail("x11 forwarding needs a live client to host the tunnel, and a detached open has none")
	}
	if ss.X11 != nil {
		oer.SetX11Enabled(true) // discriminator first
		oer.SetX11(*ss.X11)     // relay the whole X11Forward block verbatim
	}
	if ss.Prompt != "" {
		// The prompt has no bound that fits one datagram (A-D17): it rides its
		// own send stream, written and EOF'd before the request that names it,
		// in sendAssign's order. At most protocol.PromptMax bytes into a fresh
		// stream's buffer, so this does not block the caller (Tick).
		ps := runnerConn.CreateSendStream()
		if ps == nil {
			return fail("create prompt stream failed")
		}
		body := protocol.PromptBody{}
		body.SetPrompt([]byte(ss.Prompt))
		enc, err := body.EncodeCopy(nil)
		if err != nil {
			return fail("encode prompt: " + err.Error())
		}
		if err := ps.AppendData(false, enc); err != nil {
			return fail("write prompt stream: " + err.Error())
		}
		if err := ps.AppendData(true); err != nil {
			return fail("close prompt stream: " + err.Error())
		}
		oer.SetPromptPresent(true)
		oer.SetPromptStreamId(uint64(ps.ID()))
	}
	rreq.SetOpenExec(oer)
	if err := sendRunnerRequest(runnerConn, &rreq); err != nil {
		return fail("send open_exec to runner: " + err.Error())
	}

	// Spawn a SessionMux that owns the runner stream and supports
	// attach/detach of successive TUI streams — every interactive session is
	// detachable. A nil Sessions registry is a programming error.
	if h.Sessions == nil {
		return fail("Sessions registry not wired (programming error)")
	}
	ringSize := h.RingBufferSize
	if ringSize <= 0 {
		ringSize = 1 << 20 // 1 MiB default
	}
	parentCtx := h.Ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	kind := protocol.TaskKind_Interactive
	if ss.EventStream {
		kind = protocol.TaskKind_Stream
	}
	mux := NewSessionMux(parentCtx, taskIDHex, kind, runnerStream, NewRingBuffer(ringSize), h.sessionHooks())
	h.Sessions.Add(taskIDHex, mux)

	if ss.Detached {
		// Nobody attaches, so the OnDetach hook that moves an attached open to
		// Detached when its client leaves never fires; set it here.
		_ = h.Tasks.SetDetached(taskIDHex)
		// The size a client would have sent on its stream, applied as that
		// stream's first frame would have been.
		if ss.Rows != 0 && ss.Cols != 0 {
			if err := mux.applyWinSizeFrame(winSizeFrame(ss.Rows, ss.Cols)); err != nil {
				slog.Warn("startSession: initial size", "task", taskIDHex, "err", err)
			}
		}
	} else if err := mux.Attach(parentCtx, tuiStream); err != nil {
		mux.Stop()
		h.Sessions.Remove(taskIDHex)
		finishWithError("initial attach failed: " + err.Error())
		return nil, internal
	}

	if t, ok := h.Tasks.Get(taskIDHex); ok && t.RunMode == protocol.RunMode_Background {
		h.armRunWatcher(taskIDHex, mux)
	}

	// The SessionMux's onStop hook handles Sessions.Remove. When the mux stops
	// (runner EOF or Stop()), also clean up task state and the registry
	// binding — see afterMuxStopped for the exact rules.
	go func() {
		<-mux.Wait()
		h.afterMuxStopped(taskIDHex, runner.ID)
	}()
	return tuiStream, protocol.OpenInteractiveStatus_Ok
}

// StartQueuedSession is Tick's start for a queued background session task,
// already Assigned to runnerID and bound on it: a detached session with the
// task's own prompt, profile, args and PTY size. A non-nil error means the
// start failed and the task is already Failed (startSession finished it).
func (h *TaskHandler) StartQueuedSession(runnerID objproto.ConnectionID, taskID string) error {
	t, ok := h.Tasks.Get(taskID)
	if !ok {
		return fmt.Errorf("start queued session %s: no such task", taskID)
	}
	runner, ok := h.Registry.Get(runnerID)
	if !ok {
		h.Tasks.Finish(taskID, -1, []byte("server: runner went away before the session started"))
		h.Registry.UnbindTask(runnerID, taskID)
		return fmt.Errorf("start queued session %s: runner %v gone", taskID, runnerID)
	}
	_, st := h.startSession(sessionStart{
		TaskIDHex:          taskID,
		Runner:             runner,
		Profile:            t.AgentProfile,
		Repo:               t.RepoPath,
		Args:               t.ExtraArgs,
		ResumeConversation: t.ResumeConversation,
		EventStream:        t.Kind == protocol.TaskKind_Stream,
		Detached:           true,
		Rows:               t.Cond.Rows,
		Cols:               t.Cond.Cols,
		Prompt:             t.Prompt,
	})
	if st != protocol.OpenInteractiveStatus_Ok {
		return fmt.Errorf("start queued session %s: %v", taskID, st)
	}
	return nil
}

// armRunWatcher arms the background run's condition watcher on its mux.
// Pillar A2 Task 3 stub; Task 5 replaces it within the same landing.
func (h *TaskHandler) armRunWatcher(taskID string, mux *SessionMux) {}
