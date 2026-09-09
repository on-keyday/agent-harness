package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Holding tasks across a DELIBERATE server restart.
// docs/superpowers/specs/2026-09-10-task-hold-across-server-restart-design.md
//
// Only a deliberate shutdown holds. A crash sends nothing, so it recovers
// nothing and leaves exactly the pre-change behaviour — that asymmetry is the
// design, not a scope cut: an automatic hold could not tell a shutdown from a
// partition to a server that is still running, and would sit on children
// nobody will ever re-adopt.

// holdDrainQuiet is how long a held session's output stream must be silent
// before its screen is captured.
//
// There is no EOF to wait for: nobody closes anything, the runner has simply
// stopped forwarding. And the ack cannot order itself against those frames —
// it rides a different trsf stream — so the boundary has to be observed rather
// than signalled. runnerPump stamps lastOutput per frame, which is exactly the
// observation needed.
const holdDrainQuiet = 60 * time.Millisecond

// defaultHoldWindow / defaultHoldAckTimeout are the flag defaults, kept here
// beside the mechanism they bound rather than in the flag block.
const (
	defaultHoldWindow     = 90 * time.Second
	defaultHoldAckTimeout = 1500 * time.Millisecond
)

// holdDrainMax bounds the wait regardless, because the whole sequence lives
// inside a hard-kill window this design does not own (see §5's ceiling).
const holdDrainMax = 400 * time.Millisecond

// holdScreenDir is where a held interactive session's repaint program is kept
// between the two servers. Beside the WAL, since it is restart state with the
// same lifetime.
func holdScreenDir(dataDir string) string { return filepath.Join(dataDir, "held") }

func holdScreenPath(dataDir, taskIDHex string) string {
	return filepath.Join(holdScreenDir(dataDir), taskIDHex+".screen")
}

// deliverHoldTasksAck routes a runner's ack to the shutdown sequence waiting
// for it, keyed by the runner IDENTITY rather than by a request id: one hold
// goes to every registered runner and each answers exactly once, so the
// identity is already the correlation key.
func (s *Server) deliverHoldTasksAck(identityHex string, ack protocol.HoldTasksAck) {
	s.holdRespMu.Lock()
	ch, ok := s.holdRespCh[identityHex]
	s.holdRespMu.Unlock()
	if !ok {
		return // an ack for a hold that already timed out, or no hold at all
	}
	select {
	case ch <- ack:
	default:
	}
}

// heldSet is what one runner committed to keeping, by task id hex.
type heldSet struct {
	identity protocol.RunnerID
	tasks    map[string]bool
}

// RunHoldSequence is PHASE 1 of the hold: ask every registered runner to keep
// its children, learn which ones it will keep, capture the screens, and record
// the result. Returns the number of tasks held.
//
// It takes its OWN context on purpose. Both shutdown triggers cancel the
// server's root context — signal.NotifyContext and the --shutdown-file watcher
// both call cancel() — so a hold that sent on that context would send on a dead
// one, hold nothing, and say nothing. It would also pass any test that calls
// this function directly, because only the real path arrives with the context
// already cancelled.
func (s *Server) RunHoldSequence() int {
	if s.cfg.HoldWindow <= 0 {
		return 0
	}
	if s.cfg.DataDir == "" {
		// Nowhere to write task_held. Holding children whose records cannot be
		// persisted would kill them at re-adoption after a pointless window.
		s.cfg.Logger.Info("hold: skipped, no --data-dir")
		return 0
	}
	log := s.cfg.Logger
	// Set BEFORE anything goes out, not after the acks come back.
	//
	// The window that matters is between a runner arming its hold and this
	// server writing task_held: in it the tasks are still Running, so a
	// reconnecting runner's report is refused as "not held" and the empty
	// accepted list it gets back reads as "none of yours survives" — and it
	// kills the children. Marking the whole sequence is what makes the answer
	// "nobody looked" for its entire duration.
	s.shuttingDown.Store(true)
	ackTimeout := s.cfg.HoldAckTimeout
	if ackTimeout <= 0 {
		ackTimeout = defaultHoldAckTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()

	var hold protocol.HoldID
	if _, err := rand.Read(hold.Id[:]); err != nil {
		log.Error("hold: could not mint a hold id", "err", err)
		return 0
	}
	holdHex := hex.EncodeToString(hold.Id[:])
	deadline := time.Now().Add(s.cfg.HoldWindow).UnixNano()

	runners := s.registry.List()
	if len(runners) == 0 {
		return 0
	}

	// Fan out, then collect. Every runner is asked in parallel: the window is
	// one timeout for the whole fleet, not one per runner.
	s.holdRespMu.Lock()
	if s.holdRespCh == nil {
		s.holdRespCh = make(map[string]chan protocol.HoldTasksAck)
	}
	chans := make(map[string]chan protocol.HoldTasksAck, len(runners))
	for _, r := range runners {
		if r.Conn == nil || r.Identity.IsZero() {
			continue
		}
		idHex := r.Identity.Hex()
		ch := make(chan protocol.HoldTasksAck, 1)
		s.holdRespCh[idHex] = ch
		chans[idHex] = ch
	}
	s.holdRespMu.Unlock()
	defer func() {
		s.holdRespMu.Lock()
		for idHex := range chans {
			delete(s.holdRespCh, idHex)
		}
		s.holdRespMu.Unlock()
	}()

	req := protocol.HoldTasksRequest{HoldId: hold, HoldMs: uint32(s.cfg.HoldWindow / time.Millisecond)}
	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_HoldTasks
	rr.SetHoldTasks(req)
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		log.Error("hold: encode failed", "err", err)
		return 0
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	acked := make([]heldSet, 0, len(chans))
	for _, r := range runners {
		if r.Conn == nil || r.Identity.IsZero() {
			continue
		}
		entry := r
		idHex := entry.Identity.Hex()
		ch, ok := chans[idHex]
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := entry.Conn.SendMessage(payload); err != nil {
				log.Warn("hold: send failed", "runner", idHex, "err", err)
				return
			}
			select {
			case <-ctx.Done():
				// A runner that does not ack in time holds nothing: its tasks
				// take the normal path. Loss here fails safe.
				log.Warn("hold: no ack in time", "runner", idHex)
			case ack := <-ch:
				if ack.HoldId.Id != hold.Id {
					log.Warn("hold: ack for a different hold", "runner", idHex)
					return
				}
				set := heldSet{identity: entry.Identity, tasks: make(map[string]bool, len(ack.Tasks))}
				for _, t := range ack.Tasks {
					set.tasks[hex.EncodeToString(t.Id[:])] = true
				}
				mu.Lock()
				acked = append(acked, set)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Merge by INTERSECTION, not union: an acked pair counts only if the store
	// says that task is assigned to that identity. Same check the re-adoption
	// gate makes, applied at the near end so a bad entry never reaches disk.
	type holdPair struct {
		taskID   string
		identity protocol.RunnerID
	}
	pairs := make([]holdPair, 0, 16)
	for _, set := range acked {
		for taskID := range set.tasks {
			t, ok := s.tasks.Get(taskID)
			if !ok {
				log.Warn("hold: ack named an unknown task", "task", taskID)
				continue
			}
			if t.AssignedTo != set.identity {
				log.Warn("hold: ack named a task assigned elsewhere",
					"task", taskID, "acked_by", set.identity.Hex(), "assigned_to", t.AssignedTo.Hex())
				continue
			}
			pairs = append(pairs, holdPair{taskID: taskID, identity: set.identity})
		}
	}
	if len(pairs) == 0 {
		return 0
	}

	// Drain, THEN capture. runnerPump is a synchronous read -> model -> ring
	// loop that abandons whatever is unread when its context dies, so a
	// snapshot taken before the drain misses every byte the child wrote
	// between the capture and the runner's ack — bytes that sit in no buffer
	// anywhere.
	ids := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ids = append(ids, p.taskID)
	}
	s.drainHeldSessions(ids)
	if err := os.MkdirAll(holdScreenDir(s.cfg.DataDir), 0o755); err != nil {
		log.Warn("hold: could not create the screen dir", "err", err)
	}
	for _, p := range pairs {
		s.captureHeldScreen(p.taskID)
	}

	held := 0
	for _, p := range pairs {
		if err := s.tasks.MarkHold(p.taskID, p.identity.Hex(), holdHex, deadline); err != nil {
			// The commonest cause is the race this refusal exists for: the
			// task finished while acks were being collected.
			log.Info("hold: not held", "task", p.taskID, "err", err)
			continue
		}
		held++
	}
	log.Info("hold: armed", "hold_id", holdHex, "tasks", held,
		"window", s.cfg.HoldWindow, "deadline_ns", deadline)

	// LAST, not earlier. Closing the door goes through httpServer.Shutdown,
	// which waits out the connection grace period and then blocks on the serve
	// loop — so calling it mid-sequence swallowed the rest of the shutdown
	// budget and the process exited before a single task_held was written.
	// Measured: a WAL holding only task_created and task_assigned, a task that
	// replayed as an ordinary interrupted one, and a child the runner had
	// faithfully kept alive with nobody left to claim it.
	//
	// A runner that reconnects during the writes above is covered by the
	// declined bit instead, which is the case that field exists for.
	s.closeListeners()
	return held
}

// closeListeners stops accepting new connections without touching live ones.
// Idempotent: the shutdown path may also reach it.
func (s *Server) closeListeners() {
	s.stopAcceptingMu.Lock()
	fn := s.stopAcceptingFn
	s.stopAcceptingMu.Unlock()
	if fn == nil {
		return
	}
	s.stopAcceptingOnce.Do(fn)
}

// SetStopAccepting registers the listener-closing hook. serve() supplies it.
func (s *Server) SetStopAccepting(fn func()) {
	s.stopAcceptingMu.Lock()
	s.stopAcceptingFn = fn
	s.stopAcceptingMu.Unlock()
}

// drainHeldSessions waits for each held session's inbound frames to stop
// arriving, so the mux's screen model has applied everything the runner sent
// before it stopped forwarding.
func (s *Server) drainHeldSessions(taskIDs []string) {
	if s.taskHandler == nil || s.taskHandler.Sessions == nil || len(taskIDs) == 0 {
		return
	}
	deadline := time.Now().Add(holdDrainMax)
	for {
		quiet := true
		now := time.Now().UnixNano()
		for _, id := range taskIDs {
			mux := s.taskHandler.Sessions.Get(id)
			if mux == nil {
				continue
			}
			if last := mux.LastOutputUnixNano(); last != 0 && now-last < int64(holdDrainQuiet) {
				quiet = false
				break
			}
		}
		if quiet || time.Now().After(deadline) {
			return
		}
		time.Sleep(holdDrainQuiet / 3)
	}
}

// captureHeldScreen writes the task's screen as a repaint program, which is
// what re-adoption replays into the rebuilt mux. Best effort: a held task with
// no snapshot falls back to the runner's resize nudge, which is worse than a
// snapshot and much better than not holding.
func (s *Server) captureHeldScreen(taskIDHex string) {
	if s.taskHandler == nil || s.taskHandler.Sessions == nil {
		return
	}
	mux := s.taskHandler.Sessions.Get(taskIDHex)
	if mux == nil {
		// A oneshot has none, which is ordinary. An INTERACTIVE task with no
		// mux here is not: it means the session registry was already emptied,
		// and the rebind that follows will have no screen to replay.
		s.cfg.Logger.Info("hold: no session mux to capture", "task", taskIDHex)
		return
	}
	rp := mux.screenRepaint()
	if len(rp) == 0 {
		s.cfg.Logger.Warn("hold: screen repaint was empty", "task", taskIDHex)
		return
	}
	path := holdScreenPath(s.cfg.DataDir, taskIDHex)
	if err := os.WriteFile(path, rp, 0o644); err != nil {
		s.cfg.Logger.Warn("hold: screen capture failed", "task", taskIDHex, "err", err)
		return
	}
	s.cfg.Logger.Info("hold: screen captured", "task", taskIDHex, "bytes", len(rp))
}

// readHeldScreen returns a held task's captured screen and removes the file.
// Removing it on read is what keeps a stale snapshot from repainting a LATER
// session with a screen from before the restart.
func (s *Server) readHeldScreen(taskIDHex string) []byte {
	if s.cfg.DataDir == "" {
		return nil
	}
	path := holdScreenPath(s.cfg.DataDir, taskIDHex)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_ = os.Remove(path)
	return b
}

// sweepHeldScreens deletes captures whose task is not Held after a replay: a
// snapshot for a task that was never held, or that has since been re-adopted
// and fed, has no reader. This is what makes a speculative capture free.
func (s *Server) sweepHeldScreens() {
	if s.cfg.DataDir == "" {
		return
	}
	dir := holdScreenDir(s.cfg.DataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".screen" {
			continue
		}
		id := name[:len(name)-len(".screen")]
		if t, ok := s.tasks.Get(id); ok && t.Status == protocol.TaskStatus_Held {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// expireHeldTasks fails every task whose hold deadline has passed, and returns
// the earliest deadline still outstanding (zero when none is).
//
// One timer, not a sweeper: every task held by a given shutdown shares that
// shutdown's deadline, so a replay knows the whole schedule up front.
func (s *Server) expireHeldTasks(now int64) int64 {
	var earliest int64
	for _, t := range s.tasks.List(0) {
		if t.Status != protocol.TaskStatus_Held {
			continue
		}
		if t.HoldDeadline != 0 && t.HoldDeadline <= now {
			s.tasks.FailHeld(t.ID, "hold_expired")
			continue
		}
		if t.HoldDeadline != 0 && (earliest == 0 || t.HoldDeadline < earliest) {
			earliest = t.HoldDeadline
		}
	}
	return earliest
}

// watchHeldDeadlines arms a single timer for the earliest outstanding hold
// deadline and re-arms after each firing. A startup pass runs first, the shape
// the auto-prune block uses.
func (s *Server) watchHeldDeadlines(ctx context.Context) {
	next := s.expireHeldTasks(time.Now().UnixNano())
	for next != 0 {
		d := time.Until(time.Unix(0, next))
		if d < 0 {
			d = 0
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		next = s.expireHeldTasks(time.Now().UnixNano())
	}
}
