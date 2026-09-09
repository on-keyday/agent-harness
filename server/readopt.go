package server

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/appwire"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Re-adoption: PHASE 3 of the hold.
// docs/superpowers/specs/2026-09-10-task-hold-across-server-restart-design.md
//
// A runner that reconnects still holding children reports them in its
// RunnerHello, and this decides which of them come back. It runs at the
// identity gate rather than as a message after the handshake, so registration
// and re-adoption are ONE decision: the alternative needs the server to hold a
// window open before it may fail held tasks, which is a new timing dependency
// in exactly the path where a missed window means a killed session.

// ReadoptResult is what a runner is told: the ids the server accepted. The
// runner kills every held child not named here.
type ReadoptResult struct {
	Accepted []protocol.TaskID
	// Declined says this server did not reconcile the report at all (it is
	// shutting down). The runner must NOT read the empty accepted list as a
	// refusal — nobody looked.
	Declined bool
	// Rebind lists the accepted tasks that need a fresh session stream, i.e.
	// the interactive ones. The server knows each task's kind, so the runner
	// never reports it.
	Rebind []string
}

// readoptHeldTasks reconciles a reconnecting runner's held-task report against
// the log.
//
// Accepts an entry only when all three hold: the task exists and is Held, its
// hold id matches the one the runner echoes, and the identity the log recorded
// is the identity that just connected. The first two are what stop a stale
// report from a previous hold; the third is what stops a runner naming a task
// that was never its own.
//
// Nothing here takes AUTHORITY from the runner. caps, scope, worktree, profile
// and args are already in the store from the log. The one thing that must come
// off the wire is the agentboard ticket, because the board's registry is an
// in-memory map that a restart forgets while the surviving agent keeps
// presenting the value frozen into its env — re-registering a freshly minted
// ticket would answer BadTicket and leave a live agent unable to use the board
// at all. The runner already knows that value (it wrote the env), so reporting
// it grants a capability it has by construction.
func (s *Server) readoptHeldTasks(identity protocol.RunnerID, report protocol.HeldTasksReport) ReadoptResult {
	log := s.cfg.Logger
	var out ReadoptResult
	if identity.IsZero() {
		return out
	}
	reportHold := hex.EncodeToString(report.HoldId.Id[:])
	acceptedIDs := make(map[string]bool, len(report.Tasks))

	for _, ht := range report.Tasks {
		taskID := hex.EncodeToString(ht.TaskId.Id[:])

		t, ok := s.tasks.Get(taskID)
		switch {
		case !ok:
			log.Warn("readopt: refused, unknown task", "task", taskID, "runner", identity.Hex())
			continue
		case t.Status != protocol.TaskStatus_Held:
			log.Warn("readopt: refused, not held", "task", taskID, "status", t.Status.String())
			continue
		case t.HoldID != reportHold:
			log.Warn("readopt: refused, hold id mismatch",
				"task", taskID, "reported", reportHold, "recorded", t.HoldID)
			continue
		case t.AssignedTo != identity:
			log.Warn("readopt: refused, assigned to another runner",
				"task", taskID, "reported_by", identity.Hex(), "assigned_to", t.AssignedTo.Hex())
			continue
		}

		// Capacity first: a re-adopted task occupies a slot on the new
		// connection, which the identity change deliberately left as this
		// change's problem.
		s.registry.BindTask(identity.Hex(), taskID)

		// Then the board, through the FUNNEL. registry.Register alone would
		// write the ticket and stop: RegisterTask also creates the taskState
		// and seeds the task's own inbound topic, without which the credential
		// validates while `agent send` matches no subscriber — a working agent
		// nobody can reach, which is worse than BadTicket.
		boardRegisterTask(s.Board, identity, taskID, ht.Ticket, t.AgentProfile)

		// A live child and no client is what Detached means, so that is where a
		// session lands — in one transition, which is also the one this task's
		// task_readopted event carries. A oneshot has no client to be missing
		// and lands Running.
		session := t.Kind == protocol.TaskKind_Interactive || t.Kind == protocol.TaskKind_Stream
		if err := s.tasks.MarkReadopted(taskID, identity.Hex(), reportHold, session); err != nil {
			log.Error("readopt: MarkReadopted failed", "task", taskID, "err", err)
			continue
		}
		if session {
			out.Rebind = append(out.Rebind, taskID)
		}
		acceptedIDs[taskID] = true
		out.Accepted = append(out.Accepted, ht.TaskId)
		log.Info("readopt: accepted", "task", taskID, "runner", identity.Hex(),
			"kind", t.Kind.String())
	}

	// The accepted set is authoritative in BOTH directions, which is the same
	// polarity the response carries. Any Held task of this identity that is
	// not in it is over — whether the report omitted it (the runner did not
	// keep it) or the report named it and was refused (the runner will kill
	// that child, because the id is absent from the accepted list it is about
	// to receive). Leaving a refused entry Held would show the operator a held
	// task for the rest of the window while its child was already dead.
	for _, t := range s.tasks.List(0) {
		if t.Status != protocol.TaskStatus_Held || t.AssignedTo != identity {
			continue
		}
		if acceptedIDs[t.ID] {
			continue
		}
		log.Info("readopt: failing a held task that was not accepted", "task", t.ID)
		s.tasks.FailHeld(t.ID, "not_held_by_runner")
	}
	return out
}

// heldReportOf is the report a hello carries, or the zero value. Isolated so
// the registration path does not have to know whether the field is populated.
func heldReportOf(hello *protocol.RunnerHello) protocol.HeldTasksReport {
	if hello == nil {
		return protocol.HeldTasksReport{}
	}
	return hello.Held
}

// rebindHeldSessions rebuilds the server side of every re-adopted interactive
// session: a fresh stream toward the runner, a fresh SessionMux around it, the
// captured screen fed in FIRST, and then a RebindSessionRequest telling the
// runner to splice the PTY it is already holding onto that stream.
//
// The ordering is the whole point. The snapshot goes in before the runner is
// told to resume, so the bytes the child produced during the gap — which the
// kernel buffered rather than dropping, because the runner stopped draining —
// arrive AFTER it and paint on top. Feed them the other way round and the
// snapshot overwrites newer output, which a test with an idle child cannot
// tell apart.
func (s *Server) rebindHeldSessions(identity protocol.RunnerID, taskIDs []string) {
	log := s.cfg.Logger
	entry, ok := s.registry.GetByIdentity(identity)
	if !ok || entry.Conn == nil {
		log.Warn("rebind: runner went away before the rebind", "runner", identity.Hex())
		return
	}
	if s.taskHandler == nil || s.taskHandler.Sessions == nil {
		return
	}
	ringSize := s.taskHandler.RingBufferSize
	if ringSize <= 0 {
		ringSize = 1 << 20
	}
	parentCtx := s.taskHandler.Ctx
	if parentCtx == nil {
		return
	}

	for _, taskID := range taskIDs {
		runnerStream := entry.Conn.CreateBidirectionalStream()
		if runnerStream == nil {
			log.Error("rebind: could not create a runner stream", "task", taskID)
			s.tasks.MarkFailed(taskID, "rebind_no_stream")
			continue
		}
		hooks := s.taskHandler.sessionHooks()
		mux := NewSessionMux(parentCtx, taskID, runnerStream, NewRingBuffer(ringSize), hooks)

		// The capture goes in BEFORE the registry insert and before the runner
		// is told to resume, and both orderings are load-bearing. Registry
		// first would let an observer attach against an empty screen model and
		// be sent a blank repaint; telling the runner first would let the gap's
		// buffered output paint UNDER the restored screen instead of on top.
		//
		// Applying the size frame is also what makes the runner stream exist
		// for the peer. A trsf stream the server creates is not findable until
		// something crosses it, and the runner's side of a rebind is a lookup
		// that waits — with nothing written this deadlocks: the runner waits
		// for a stream the server will only populate once the child produces
		// output, and the child cannot, because its relay is still parked. Over
		// WebSocket it happened to work; over UDP it failed every time with
		// "stream lookup failed", and the session came back with a live child
		// and a blank screen.
		gotSize, gotScreen, err := mux.loadHeldCapture(s.readHeldScreen(taskID))
		if err != nil {
			log.Error("rebind: applying the capture failed", "task", taskID, "err", err)
		}
		// Logged either way: this is the boundary where a re-adopted session
		// comes back blank, so what the capture carried is answered in the log
		// rather than by re-deriving it from a snapshot afterwards.
		if gotSize && gotScreen {
			log.Info("rebind: capture applied", "task", taskID)
		} else {
			log.Warn("rebind: incomplete capture", "task", taskID,
				"size", gotSize, "screen", gotScreen)
		}
		s.taskHandler.Sessions.Add(taskID, mux)

		var rr protocol.RunnerRequest
		rr.Kind = protocol.RunnerRequestType_RebindSession
		var tid protocol.TaskID
		raw, _ := hex.DecodeString(taskID)
		copy(tid.Id[:], raw)
		rr.SetRebindSession(protocol.RebindSessionRequest{
			TaskId:   tid,
			StreamId: uint64(runnerStream.ID()),
		})
		payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
		if err != nil {
			log.Error("rebind: encode failed", "task", taskID, "err", err)
			continue
		}
		if _, _, err := entry.Conn.SendMessage(payload); err != nil {
			log.Error("rebind: send failed", "task", taskID, "err", err)
			s.tasks.MarkFailed(taskID, "rebind_send_failed")
			continue
		}
		log.Info("rebind: sent", "task", taskID, "stream", runnerStream.ID())
	}
}
