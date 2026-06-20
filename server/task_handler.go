package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/objproto"
)

// TaskHandler decodes inbound TaskControlRequest payloads from the CLI
// and replies with a TaskControlResponse. It also triggers the scheduler
// via OnChange after mutating operations (Submit, Cancel).
type TaskHandler struct {
	Tasks    *TaskStore
	Registry *Registry
	OnChange func() // called after Submit / Cancel mutations

	// NotifyHook is the configured external command for the egress leg of
	// notify (empty = egress disabled). See server/notify_hook.go.
	NotifyHook string

	// OnNotify runs the live leg for a notify (ring append + topic publish).
	// nil-safe: tests may leave it nil to exercise egress in isolation.
	OnNotify func(ev protocol.NotifyEvent)

	// remoteForwardsOnce guards the lazy init of remoteForwards so concurrent
	// callers racing on the first use cannot create two separate registries or
	// observe a torn pointer write.
	remoteForwardsOnce sync.Once
	// remoteForwards tracks active ssh -R registrations (forwardId →
	// registration). Lazily initialised via rforwards() (guarded by
	// remoteForwardsOnce) so struct-literal construction in tests need not set it.
	remoteForwards *remoteForwardRegistry

	// PruneFn handles a CLI-driven prune request. If nil, prune requests reply
	// with all-zero counts. Server.New wires this to a closure that dispatches
	// to TaskStore.PruneTerminal (time mode) or TaskStore.PruneByIDs (id mode).
	PruneFn func(req *protocol.PruneTasksRequest) (removed, skippedActive, skippedMissing int)

	// LogsDir is the directory containing per-task log files
	// (<LogsDir>/<task-id>.log). Empty disables GetTaskLog responses
	// (always returns Found=0). Server.New wires it from cfg.DataDir.
	LogsDir string

	// Board is the agentboard instance for ticket lifecycle management.
	// When nil, ticket registration is skipped (safe for tests that do not wire a Board).
	Board *agentboard.Board

	// Sessions is the registry for active detachable SessionMux instances.
	// Required for the Detachable=1 branch of handleOpenInteractive.
	// When nil, Detachable=1 requests fall through to an InternalError response.
	Sessions *SessionRegistry

	// Ctx is the server's root context, used as the parent for SessionMux
	// instances (which must outlive any single request).
	// When nil, context.Background() is used (safe for tests that don't need
	// cancellation propagation).
	Ctx context.Context

	// RingBufferSize is the capacity of the RingBuffer allocated for each
	// detachable session. When zero, defaults to 1 MiB (1 << 20 bytes).
	RingBufferSize int

	// Endpoint is the server's objproto Endpoint, used by the DialRunner
	// handler to initiate outbound ECDH handshakes. Required only when
	// handling TaskControlKind_DialRunner; safe to leave nil in tests that
	// don't exercise that path.
	Endpoint objproto.Endpoint

	// OnDialed is called by the DialRunner handler on a successful dial with
	// the server root context, the newly-active objproto.Connection, and optional
	// via-registration metadata (non-nil only for Phase C HandleWithVia calls).
	// Server.New wires this to: func(ctx, conn, viaInfo) { stash viaInfo; go s.handleConnection(ctx, conn) }
	// Safe to leave nil in tests.
	OnDialed func(ctx context.Context, conn objproto.Connection, viaInfo *ViaRegistrationInfo)

	// ResolveVia resolves a via-relay CID against the registered runners.
	// Server.Run wires this to Registry.GetByConnectionID. Tests that exercise
	// the via-relay branch (TaskControlKind_DialRunner with non-empty Via)
	// wire a stub directly.
	ResolveVia func(cid objproto.ConnectionID) (*RunnerEntry, bool)

	// ViaSendEstablishRelay sends an EstablishRelayRequest to the resolved
	// proxy_runner and blocks for the EstablishRelayResponse. Server.Run wires
	// this to Server.sendEstablishRelayRequest, which uses a per-conn response
	// channel registered before send.
	ViaSendEstablishRelay func(ctx context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error)

	// OnAgentHello validates+establishes an agent principal for a kind=agent
	// ClientHello. Implemented by Server (Validate + Board.Attach). nil => identity
	// not established (minimal test wiring); ClientHello falls back to Ok.
	OnAgentHello func(conn ConnHandle, info *protocol.AgentInfo) protocol.ClientHelloStatus

	// clientKinds maps connection ID → the kind of client that announced
	// itself via ClientHello on that connection. Submit / OpenInteractive
	// look it up to attribute task origin (ClientKind_Cli / Tui / Webui).
	// Entries are added on ClientHello and never explicitly removed; for
	// individual-dogfood scale the leak is acceptable. Tasks created on a
	// connection that never sent ClientHello get ClientKind_Unspecified
	// (the zero value of a missing map entry).
	clientKindsMu sync.Mutex
	clientKinds   map[string]protocol.ClientKind
	// principals maps connection ID → the TaskID of the agent principal for
	// kind=agent connections. Populated at ClientHello and used at Create time
	// to record CreatorTaskID on agent-submitted tasks.
	principals map[string]protocol.TaskID
}

// lookupClientKind returns the ClientKind associated with connID.
// Unknown connections return ClientKind_Unspecified (zero value).
func (h *TaskHandler) lookupClientKind(connID string) protocol.ClientKind {
	h.clientKindsMu.Lock()
	defer h.clientKindsMu.Unlock()
	return h.clientKinds[connID]
}

// lookupPrincipal returns the agent principal TaskID associated with connID.
// Unknown connections (or non-agent connections) return a zero TaskID.
func (h *TaskHandler) lookupPrincipal(connID string) protocol.TaskID {
	h.clientKindsMu.Lock()
	defer h.clientKindsMu.Unlock()
	return h.principals[connID]
}

// denyTaskControl rejects a capability-gated request with a typed
// PermissionDenied response carrying the requested kind and the missing cap.
func (h *TaskHandler) denyTaskControl(conn ConnHandle, reqKind protocol.TaskControlKind, requestID uint32, required protocol.Capability) {
	slog.Warn("capability denied", "kind", reqKind, "required_cap", required)
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_PermissionDenied, RequestId: requestID}
	resp.SetPermissionDenied(protocol.PermissionDeniedResponse{RequestedKind: reqKind, RequiredCap: required})
	out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
	conn.SendMessage(out) //nolint:errcheck
}

// Handle decodes a TaskControlRequest payload (bytes after the wire-kind byte) and replies via conn.SendMessage.
// Decode failures are logged and silently dropped (no panic).
func (h *TaskHandler) Handle(conn ConnHandle, payload []byte) {
	var req protocol.TaskControlRequest
	if err := req.DecodeExact(payload); err != nil {
		slog.Error("TaskHandler: failed to decode TaskControlRequest", "error", err)
		return
	}

	cid := conn.ConnectionID().String()
	if want, gated := requiredCap[req.Kind]; gated {
		if !hasCap(h.callerCaps(cid), want) {
			h.denyTaskControl(conn, req.Kind, req.RequestId, want)
			return
		}
	}

	switch req.Kind {
	case protocol.TaskControlKind_Submit:
		sub := req.Submit()
		if sub == nil {
			slog.Error("TaskHandler: Submit variant is nil")
			return
		}
		origin := h.lookupClientKind(cid)
		creator := h.lookupPrincipal(cid)
		creatorCaps := h.callerCaps(cid)
		submitResp := h.handleSubmit(sub, origin, creator, creatorCaps)

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Submit, RequestId: req.RequestId}
		resp.SetSubmit(submitResp)

		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

		if submitResp.Status == protocol.SubmitStatus_Ok && h.OnChange != nil {
			h.OnChange()
		}

	case protocol.TaskControlKind_List:
		h.handleList(conn, req.RequestId, cid)

	case protocol.TaskControlKind_Cancel:
		can := req.Cancel()
		if can == nil {
			slog.Error("TaskHandler: Cancel variant is nil")
			return
		}
		taskID := hex.EncodeToString(can.TaskId.Id[:])
		h.Tasks.Cancel(taskID)

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Cancel, RequestId: req.RequestId}
		resp.SetCancel(protocol.CancelStatus{Status: 0})

		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

		if h.OnChange != nil {
			h.OnChange()
		}

	case protocol.TaskControlKind_PruneTasks:
		pr := req.Prune()
		if pr == nil {
			slog.Error("TaskHandler: Prune variant is nil")
			return
		}
		var removed, skippedActive, skippedMissing uint32
		if h.PruneFn != nil {
			r, sa, sm := h.PruneFn(pr)
			removed = uint32(r)
			skippedActive = uint32(sa)
			skippedMissing = uint32(sm)
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_PruneTasks, RequestId: req.RequestId}
		resp.SetPrune(protocol.PruneTasksResponse{
			Removed:        removed,
			SkippedActive:  skippedActive,
			SkippedMissing: skippedMissing,
		})

		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_GetTaskLog:
		gl := req.GetLog()
		if gl == nil {
			slog.Error("TaskHandler: GetLog variant is nil")
			return
		}
		taskID := hex.EncodeToString(gl.TaskId.Id[:])
		h.handleGetTaskLog(conn, req.RequestId, taskID, cid)

	case protocol.TaskControlKind_OpenInteractive:
		oi := req.OpenInteractive()
		if oi == nil {
			slog.Error("TaskHandler: OpenInteractive variant is nil")
			return
		}
		origin := h.lookupClientKind(cid)
		creator := h.lookupPrincipal(cid)
		oiCreatorCaps := h.callerCaps(cid)
		oresp := h.handleOpenInteractive(conn, oi, origin, creator, oiCreatorCaps)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: req.RequestId}
		resp.SetOpenInteractive(oresp)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_OpenFileTransfer:
		oft := req.OpenFileTransfer()
		if oft == nil {
			slog.Error("TaskHandler: OpenFileTransfer variant is nil")
			return
		}
		need := protocol.Capability_FileWrite // Push / Delete / DirPush / DirDelete
		switch oft.Direction {
		case protocol.FileTransferDirection_Pull, protocol.FileTransferDirection_DirPull:
			need = protocol.Capability_FileRead
		}
		if !hasCap(h.callerCaps(cid), need) {
			h.denyTaskControl(conn, req.Kind, req.RequestId, need)
			return
		}
		oresp := h.handleOpenFileTransfer(conn, oft)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenFileTransfer, RequestId: req.RequestId}
		resp.SetOpenFileTransfer(oresp)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_ListFiles:
		lf := req.ListFiles()
		if lf == nil {
			slog.Error("TaskHandler: ListFiles variant is nil")
			return
		}
		{
			caps := h.callerCaps(cid)
			if !hasCap(caps, protocol.Capability_FileRead) && !hasCap(caps, protocol.Capability_FileWrite) {
				// report FileRead as the representative requirement in the denial
				h.denyTaskControl(conn, req.Kind, req.RequestId, protocol.Capability_FileRead)
				return
			}
		}
		lresp := h.handleListFiles(conn, lf)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ListFiles, RequestId: req.RequestId}
		resp.SetListFiles(lresp)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_OpenPortForward:
		pf := req.OpenPortForward()
		if pf == nil {
			slog.Error("TaskHandler: OpenPortForward variant is nil")
			return
		}
		{
			need := protocol.Capability_ForwardLocal
			if pf.Direction == protocol.PortForwardDirection_Remote {
				need = protocol.Capability_ForwardRemote
			}
			if !hasCap(h.callerCaps(cid), need) {
				h.denyTaskControl(conn, req.Kind, req.RequestId, need)
				return
			}
		}
		presp := h.handleOpenPortForward(conn, pf)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenPortForward, RequestId: req.RequestId}
		resp.SetOpenPortForward(presp)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_AttachSession:
		a := req.Attach()
		if a == nil {
			slog.Error("TaskHandler: AttachSession variant nil")
			return
		}
		aresp := h.handleAttachSession(conn, a)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AttachSession, RequestId: req.RequestId}
		resp.SetAttach(aresp)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_ClientHello:
		hello := req.ClientHello()
		if hello == nil {
			slog.Error("TaskHandler: ClientHello variant is nil")
			return
		}
		cid := conn.ConnectionID().String()
		slog.Info("client hello", "kind", hello.Kind.String(), "cid", cid)

		status := protocol.ClientHelloStatus_Ok
		if hello.Kind == protocol.ClientKind_Agent {
			if info := hello.AgentInfo(); info != nil && h.OnAgentHello != nil {
				status = h.OnAgentHello(conn, info)
			}
		}

		// Record kind for task-origin attribution only on success, so a rejected
		// agent is not attributed as kind=agent.
		if status == protocol.ClientHelloStatus_Ok {
			h.clientKindsMu.Lock()
			if h.clientKinds == nil {
				h.clientKinds = make(map[string]protocol.ClientKind)
			}
			h.clientKinds[cid] = hello.Kind
			// For agent connections, record the principal TaskID so that
			// tasks created on this connection can have CreatorTaskID set.
			if hello.Kind == protocol.ClientKind_Agent {
				if info := hello.AgentInfo(); info != nil {
					if h.principals == nil {
						h.principals = make(map[string]protocol.TaskID)
					}
					h.principals[cid] = info.TaskId
				}
			}
			h.clientKindsMu.Unlock()
		}

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ClientHello, RequestId: req.RequestId}
		resp.SetClientHello(protocol.ClientHelloResponse{Status: status})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_Notify:
		h.handleNotify(conn, &req)

	case protocol.TaskControlKind_DialRunner:
		dr := req.DialRunner()
		if dr == nil {
			slog.Error("TaskHandler: DialRunner variant is nil")
			return
		}
		dialCtx := h.Ctx
		if dialCtx == nil {
			dialCtx = context.Background()
		}
		handler := &DialRunnerHandler{
			Logger:                slog.Default(),
			Endpoint:              h.Endpoint,
			OnDialed:              h.OnDialed,
			ResolveVia:            h.ResolveVia,
			ViaSendEstablishRelay: h.ViaSendEstablishRelay,
		}
		var dialResp protocol.DialRunnerResponse
		if dr.Via.TransportLen == 0 {
			dialResp = handler.Handle(dialCtx, dr.Target)
		} else {
			dialResp = handler.HandleWithVia(dialCtx, dr.Target, dr.Via)
		}
		out := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_DialRunner, RequestId: req.RequestId}
		out.SetDialRunner(dialResp)
		bytes := out.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(bytes) //nolint:errcheck

	default:
		slog.Error("TaskHandler: unhandled kind", "kind", req.Kind)
	}
}

// handleSubmit resolves the runner selector and either enqueues the task or
// returns a synchronous error status. The four possible outcomes:
//
//   - SubmitStatus_Ok            — exactly one candidate, task created and queued
//   - SubmitStatus_NoRunner      — no candidates match (Any selector)
//   - SubmitStatus_PinnedNotFound — no candidates match (pinned selector)
//   - SubmitStatus_AmbiguousRunner — more than one candidate (error_msg lists hostnames)
//
// On Ok, the returned SubmitResponse carries the new TaskId.
func (h *TaskHandler) handleSubmit(req *protocol.SubmitRequest, origin protocol.ClientKind, creator protocol.TaskID, creatorCaps protocol.Capability) protocol.SubmitResponse {
	// Resume branch: when ResumeTaskId is non-zero the server reuses that id
	// (so the runner re-attaches the worktree to the retained `harness/<id>`
	// branch) instead of allocating a fresh one. The repo on the request is
	// ignored — the existing TaskEntry's RepoPath is authoritative because
	// that's the directory claude's session storage is keyed under.
	if !isZeroTaskID(req.ResumeTaskId) {
		return h.handleSubmitResume(req, origin)
	}

	// Wire is POSIX '/'-paths; use path.Clean (not filepath.Clean) so the
	// server stays OS-agnostic when matching runner-supplied roots.
	repo := path.Clean(string(req.RepoPath))
	cands := h.Registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_Any:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_PinnedNotFound}
	case len(cands) == 0:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_NoRunner}
	case len(cands) > 1:
		hostnames := make([]string, len(cands))
		for i, c := range cands {
			hostnames[i] = c.Hostname
		}
		msg := []byte("ambiguous: " + strings.Join(hostnames, ", "))
		resp := protocol.SubmitResponse{Status: protocol.SubmitStatus_AmbiguousRunner}
		resp.SetErrorMsg(msg)
		return resp
	}
	bound := cands[0]
	caps := intersectCaps(creatorCaps, req.RequestedCaps)
	taskIDHex := h.Tasks.Create(repo, string(req.Prompt), protocol.TaskKind_Oneshot, origin, creator, bound.ID, req.Selector, req.ExtraArgs.AsStrings(), caps)
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return protocol.SubmitResponse{Status: protocol.SubmitStatus_Ok, TaskId: tid}
}

// handleSubmitResume implements the resume_task_id branch of handleSubmit.
// Validation order:
//  1. Existing TaskEntry must exist (else resume_not_found).
//  2. Its TaskKind must be Oneshot — interactive resumes go through the
//     OpenInteractive path; a Submit asking to resume an interactive task is
//     a category mismatch that we reject up front (resume_not_found, since
//     no Oneshot entry by that id exists from this handler's perspective).
//  3. Runner candidates against the existing repo + the request's selector.
//     Same NoRunner / Ambiguous / PinnedNotFound mapping as the fresh path.
//  4. Tasks.Resume — atomic terminal-check + reset. Errors map to the new
//     resume_not_terminal / resume_not_found wire codes; the latter handles
//     the (rare) race where the entry was pruned between steps 1 and 4.
func (h *TaskHandler) handleSubmitResume(req *protocol.SubmitRequest, origin protocol.ClientKind) protocol.SubmitResponse {
	idHex := hex.EncodeToString(req.ResumeTaskId.Id[:])
	repo, kind, ok := h.Tasks.PeekRepo(idHex)
	if !ok || kind != protocol.TaskKind_Oneshot {
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_ResumeNotFound}
	}
	cands := h.Registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_Any:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_PinnedNotFound}
	case len(cands) == 0:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_NoRunner}
	case len(cands) > 1:
		hostnames := make([]string, len(cands))
		for i, c := range cands {
			hostnames[i] = c.Hostname
		}
		msg := []byte("ambiguous: " + strings.Join(hostnames, ", "))
		resp := protocol.SubmitResponse{Status: protocol.SubmitStatus_AmbiguousRunner}
		resp.SetErrorMsg(msg)
		return resp
	}
	bound := cands[0]

	if _, err := h.Tasks.Resume(idHex, string(req.Prompt), req.ExtraArgs.AsStrings(), req.Selector, bound.ID, origin); err != nil {
		switch err {
		case ResumeErrNotFound:
			return protocol.SubmitResponse{Status: protocol.SubmitStatus_ResumeNotFound}
		case ResumeErrNotTerminal:
			return protocol.SubmitResponse{Status: protocol.SubmitStatus_ResumeNotTerminal}
		default:
			resp := protocol.SubmitResponse{Status: protocol.SubmitStatus_InternalError}
			resp.SetErrorMsg([]byte(err.Error()))
			return resp
		}
	}
	return protocol.SubmitResponse{Status: protocol.SubmitStatus_Ok, TaskId: req.ResumeTaskId}
}

// isZeroTaskID reports whether t is the all-zero sentinel used by the wire
// to encode "no resume target — create a new task".
func isZeroTaskID(t protocol.TaskID) bool {
	return t.Id == [16]byte{}
}

// handleOpenInteractive resolves the runner selector, gates on capacity, and
// (when all checks pass) allocates two bidirectional streams, binds the task,
// sends OpenExec to the runner, and starts the splice goroutine.
//
// Return value is the synchronous protocol response; the caller sends it over
// tuiConn. tuiConn may be nil when called from tests that only exercise the
// error paths (NoRunnerForRepo, RunnerBusy, AmbiguousRunner, PinnedNotFound).
//
// Synchronous decision logic (four cases):
//   - PinnedNotFound   — pinned selector (Kind != Any) but no candidates match
//   - NoRunnerForRepo  — Any selector and no candidates match
//   - AmbiguousRunner  — more than one candidate matches
//   - RunnerBusy       — exactly one candidate but it is at capacity
//
// If none of the above apply, the method proceeds with stream setup. Stream
// errors (CreateBidirectionalStream, SendMessage) return InternalError.
//
// The task is registered as Running before the splice begins so the scheduler
// does not pick it up for a parallel AssignTask. The runner finalizes the task
// lifecycle by sending TaskStarted (worktree dir filled in) and TaskFinished
// (exit code from claude) over the regular RunnerControl path.
func (h *TaskHandler) handleOpenInteractive(tuiConn ConnHandle, req *protocol.OpenInteractiveRequest, origin protocol.ClientKind, creator protocol.TaskID, creatorCaps protocol.Capability) protocol.OpenInteractiveResponse {
	errResp := func(status protocol.OpenInteractiveStatus) protocol.OpenInteractiveResponse {
		slog.Error("handleOpenInteractive: rejecting request", "status", status.String(), "repo", string(req.RepoPath), "selector", req.Selector)
		return protocol.OpenInteractiveResponse{Status: status}
	}

	// Resume vs fresh: on resume we ignore req.RepoPath and use the existing
	// TaskEntry's repo, then run the same candidate-resolution gate. The
	// task entry is then transitioned via Tasks.Resume (atomic) instead of
	// Tasks.Create. The downstream stream-allocation + open_exec block is
	// shared between the two paths.
	resuming := !isZeroTaskID(req.ResumeTaskId)
	var repo string
	var existingTaskIDHex string
	if resuming {
		idHex := hex.EncodeToString(req.ResumeTaskId.Id[:])
		var kind protocol.TaskKind
		var ok bool
		repo, kind, ok = h.Tasks.PeekRepo(idHex)
		if !ok || kind != protocol.TaskKind_Interactive {
			return errResp(protocol.OpenInteractiveStatus_ResumeNotFound)
		}
		existingTaskIDHex = idHex
	} else {
		repo = path.Clean(string(req.RepoPath))
	}

	cands := h.Registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_Any:
		return errResp(protocol.OpenInteractiveStatus_PinnedNotFound)
	case len(cands) == 0:
		return errResp(protocol.OpenInteractiveStatus_NoRunnerForRepo)
	case len(cands) > 1:
		return errResp(protocol.OpenInteractiveStatus_AmbiguousRunner)
	}
	runner := cands[0]

	// Capacity gate — interactive sessions cannot queue; fail fast if runner is full.
	if len(runner.ActiveTasks) >= runner.MaxTasks {
		return errResp(protocol.OpenInteractiveStatus_RunnerBusy)
	}

	// tuiConn is nil in test invocations that only exercise the error paths above.
	// A nil conn here indicates a programming error in production callers.
	if tuiConn == nil {
		slog.Error("handleOpenInteractive: tuiConn is nil on Ok path")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}

	// Allocate or revive the task entry.
	var taskIDHex string
	if resuming {
		if _, err := h.Tasks.Resume(existingTaskIDHex, "", req.ExtraArgs.AsStrings(), req.Selector, runner.ID, origin); err != nil {
			switch err {
			case ResumeErrNotFound:
				return errResp(protocol.OpenInteractiveStatus_ResumeNotFound)
			case ResumeErrNotTerminal:
				return errResp(protocol.OpenInteractiveStatus_ResumeNotTerminal)
			default:
				return errResp(protocol.OpenInteractiveStatus_InternalError)
			}
		}
		taskIDHex = existingTaskIDHex
	} else {
		// The TaskKind_Interactive value is the authoritative marker — empty
		// prompt is incidental.
		caps := intersectCaps(creatorCaps, req.RequestedCaps)
		taskIDHex = h.Tasks.Create(repo, "", protocol.TaskKind_Interactive, origin, creator, runner.ID, req.Selector, req.ExtraArgs.AsStrings(), caps)
	}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)

	// Mark the task Running and bound to this runner immediately so the
	// scheduler doesn't try to AssignTask it. The runner will fill in the
	// real worktree dir via TaskStarted shortly after open_exec arrives.
	h.Tasks.Assign(taskIDHex, runner.ID, "")
	if req.Detachable() {
		h.Tasks.SetDetachableFlag(taskIDHex, true)
	}
	h.Registry.BindTask(runner.ID, taskIDHex)

	finishWithError := func(reason string) {
		slog.Error("handleOpenInteractive: "+reason, "task", taskIDHex, "runner", runner.Hostname)
		h.Tasks.Finish(taskIDHex, -1, []byte("server: "+reason))
		h.Registry.UnbindTask(runner.ID, taskIDHex)
		if h.Board != nil {
			h.Board.Revoke(runnerIDFromConnID(runner.ID), taskIDFromHex(taskIDHex))
		}
	}

	// Generate a fresh ticket for the agent Hello handshake.
	var ticket [16]byte
	if _, err := rand.Read(ticket[:]); err != nil {
		slog.Error("handleOpenInteractive: ticket generation failed", "task", taskIDHex, "err", err)
		finishWithError("ticket gen failed: " + err.Error())
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}
	if h.Board != nil {
		h.Board.Registry().Register(runnerIDFromConnID(runner.ID), taskIDFromHex(taskIDHex), ticket)
	}

	tuiStream := tuiConn.CreateBidirectionalStream()
	if tuiStream == nil {
		finishWithError("create client-side stream failed")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}
	runnerConn := runner.Conn
	if runnerConn == nil {
		_ = tuiStream.CloseBoth()
		finishWithError("runner conn nil")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}
	runnerStream := runnerConn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = tuiStream.CloseBoth()
		finishWithError("create runner-side stream failed")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}

	// Tell the runner to wire its stream end to claude.
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenExec}
	oer := protocol.OpenExecRunnerRequest{
		TaskId:     tid,
		StreamId:   uint64(runnerStream.ID()),
		AuthTicket: ticket,
		ExtraArgs:  req.ExtraArgs,
	}
	oer.SetRepoPath([]byte(repo))
	if req.Detachable() {
		oer.SetDetachable(true)
	}
	if req.X11Enabled() {
		oer.SetX11Enabled(true) // discriminator first
		if f := req.X11(); f != nil {
			oer.SetX11(*f) // relay the whole X11Forward block verbatim
		}
	}
	rreq.SetOpenExec(oer)
	rdata := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runnerConn.SendMessage(rdata); err != nil {
		_ = tuiStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		finishWithError("send open_exec to runner: " + err.Error())
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}

	if req.Detachable() {
		// Detachable path: spawn a SessionMux that owns the runner stream and
		// supports attach/detach of successive TUI streams. The initial TUI
		// stream (tuiStream) is attached after registration.
		//
		// Sessions must be wired before calling handleOpenInteractive with
		// Detachable=1. A nil Sessions registry is a programming error.
		if h.Sessions == nil {
			_ = tuiStream.CloseBoth()
			_ = runnerStream.CloseBoth()
			finishWithError("Sessions registry not wired (programming error)")
			return errResp(protocol.OpenInteractiveStatus_InternalError)
		}

		// RingBufferSize defaults to 1 MiB when not configured. This will be
		// made configurable via Config.DetachRingBufferSize in Task 8.
		ringSize := h.RingBufferSize
		if ringSize <= 0 {
			ringSize = 1 << 20 // 1 MiB default
		}

		hooks := SessionHooks{
			OnAttach: func(id string) { h.Tasks.MarkAttached(id, true) },
			OnDetach: func(id string) { _ = h.Tasks.SetDetached(id) },
			OnStop:   func(id string) { h.Sessions.Remove(id) },
		}

		parentCtx := h.Ctx
		if parentCtx == nil {
			parentCtx = context.Background()
		}

		mux := NewSessionMux(parentCtx, taskIDHex, runnerStream, NewRingBuffer(ringSize), hooks)
		h.Sessions.Add(taskIDHex, mux)

		if err := mux.Attach(parentCtx, tuiStream); err != nil {
			mux.Stop()
			h.Sessions.Remove(taskIDHex)
			finishWithError("initial attach failed: " + err.Error())
			return errResp(protocol.OpenInteractiveStatus_InternalError)
		}

		// The SessionMux's onStop hook (above) handles Sessions.Remove.
		// When the mux stops (runner EOF or Stop()), we also clean up task
		// state and registry binding — same defensive cleanup as the legacy path.
		go func() {
			<-mux.Wait()
			h.Sessions.Remove(taskIDHex) // NEW: defensive — handles race where OnStop fired before Sessions.Add
			if t, ok := h.Tasks.Get(taskIDHex); ok && t.Status == protocol.TaskStatus_Running {
				h.Tasks.Cancel(taskIDHex)
			}
			h.Registry.UnbindTask(runner.ID, taskIDHex)
			if h.OnChange != nil {
				h.OnChange()
			}
		}()
	} else {
		// Legacy (non-detachable) path: direct splice between TUI and runner streams.
		go func() {
			spliceBidi(tuiStream, runnerStream, taskIDHex)

			// Defensive cleanup. The expected path is that the runner's
			// ExecuteCommand returns once the splice tears down both stream
			// ends, and the runner sends TaskFinished — RunnerHandler then
			// flips the task to Succeeded/Failed and the runner back to Idle.
			//
			// But if the runner crashed mid-session, the runner connection
			// dropped, or the runner is wedged before it can reach the
			// TaskFinished step, the task we marked Running in this handler
			// (and the runner we marked Busy) would otherwise stay that way
			// forever. Finalize them here, but only if the state we set is
			// still in place — a TaskFinished that genuinely arrived first
			// already moved the task terminal and the runner Idle, and we
			// must not undo that (the scheduler may have re-bound the
			// runner to a different task by now).
			// Cancel is idempotent (skips terminal states), so a TaskFinished
			// that genuinely arrived first is unaffected. UnbindTask is
			// idempotent (no-op if the task slot is no longer present), so it
			// cannot clobber a fresh scheduler assignment that already moved the
			// runner to a different task.
			if t, ok := h.Tasks.Get(taskIDHex); ok && t.Status == protocol.TaskStatus_Running {
				h.Tasks.Cancel(taskIDHex)
			}
			h.Registry.UnbindTask(runner.ID, taskIDHex)
			if h.OnChange != nil {
				h.OnChange()
			}
		}()
	}

	return protocol.OpenInteractiveResponse{
		Status:   protocol.OpenInteractiveStatus_Ok,
		TaskId:   tid,
		StreamId: uint64(tuiStream.ID()),
	}
}

// handleAttachSession re-attaches a client to an existing detachable interactive
// session identified by req.TaskId. It validates that the task exists, is
// interactive, is detachable, is non-terminal, and has a live SessionMux.
// On success it allocates a new bidi stream toward the client and calls
// mux.Attach, which replays the ring buffer and resumes the splice.
func (h *TaskHandler) handleAttachSession(conn ConnHandle, req *protocol.AttachSessionRequest) protocol.AttachSessionResponse {
	errResp := func(s protocol.AttachSessionStatus) protocol.AttachSessionResponse {
		return protocol.AttachSessionResponse{Status: s}
	}
	idHex := hex.EncodeToString(req.TaskId.Id[:])

	info, ok := h.Tasks.Get(idHex)
	if !ok {
		slog.Warn("AttachSession: task_not_found", "task", idHex)
		return errResp(protocol.AttachSessionStatus_NotFound)
	}
	if info.Kind != protocol.TaskKind_Interactive {
		return errResp(protocol.AttachSessionStatus_NotInteractive)
	}
	if !info.Detachable {
		return errResp(protocol.AttachSessionStatus_NotDetachable)
	}
	switch info.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return errResp(protocol.AttachSessionStatus_AlreadyTerminal)
	}
	if h.Sessions == nil {
		slog.Error("AttachSession: handler missing Sessions registry")
		return errResp(protocol.AttachSessionStatus_InternalError)
	}
	mux := h.Sessions.Get(idHex)
	if mux == nil {
		return errResp(protocol.AttachSessionStatus_RunnerUnreachable)
	}

	if conn == nil {
		slog.Error("AttachSession: nil conn")
		return errResp(protocol.AttachSessionStatus_InternalError)
	}
	tuiStream := conn.CreateBidirectionalStream()
	if tuiStream == nil {
		slog.Error("AttachSession: open client stream failed", "task", idHex)
		return errResp(protocol.AttachSessionStatus_InternalError)
	}

	// Capture replay size BEFORE Attach. Informational for the client UI.
	replayBytes := uint64(mux.RingBufferLen())

	parentCtx := h.Ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	attach := mux.Attach
	if req.Mode == protocol.AttachMode_View {
		attach = mux.AttachViewer
	}
	if err := attach(parentCtx, tuiStream); err != nil {
		slog.Error("AttachSession: attach", "task", idHex, "mode", req.Mode, "err", err)
		_ = tuiStream.CloseBoth()
		return errResp(protocol.AttachSessionStatus_InternalError)
	}

	return protocol.AttachSessionResponse{
		Status:      protocol.AttachSessionStatus_Ok,
		StreamId:    uint64(tuiStream.ID()),
		ReplayBytes: replayBytes,
	}
}

// spliceBidi pumps bytes between two bidirectional streams in both directions
// until either side closes or errors. Each side's EOF is propagated to the
// other so exec.ExecuteCommand can react to TUI detach (close stream → EOF
// on PTY stdin pipe → claude exits via SIGHUP/SIGTERM/SIGKILL ladder).
//
// When either direction returns — clean EOF or error — both streams are
// fully closed via CloseBoth. For an interactive PTY, half-closes are not
// meaningful: if one direction is dead the other should be torn down too,
// otherwise the surviving relay goroutine blocks forever on ReadDirect
// (e.g. TUI vanished mid-session, claude is sitting idle waiting for
// stdin) and the splice never finishes.
func spliceBidi(a, b trsf.BidirectionalStream, taskIDHex string) {
	var wg sync.WaitGroup
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = a.CloseBoth()
			_ = b.CloseBoth()
		})
	}
	wg.Add(2)
	go func() { defer wg.Done(); defer teardown(); relayBytes(a, b) }()
	go func() { defer wg.Done(); defer teardown(); relayBytes(b, a) }()
	wg.Wait()
	slog.Info("OpenInteractive: splice ended", "task_id", taskIDHex)
}

// spliceBidiHalfClose is a non-tear-down variant of spliceBidi for request /
// response patterns over a bidi stream — like file_transfer push (client EOFs
// → runner ACKs back) and pull / list_files (runner EOFs → client closes its
// idle send side). Both relays complete on natural EOF; the streams are
// CloseBoth'd only after both have returned.
//
// Use this when the application protocol on top of the stream guarantees that
// both directions will EOF on their own. For interactive PTY (where one side
// going away should kill the other), use spliceBidi.
func spliceBidiHalfClose(a, b trsf.BidirectionalStream, taskIDHex string) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); relayBytes(a, b) }()
	go func() { defer wg.Done(); relayBytes(b, a) }()
	wg.Wait()
	_ = a.CloseBoth()
	_ = b.CloseBoth()
	slog.Info("file_transfer: splice ended", "task_id", taskIDHex)
}

// relayBytes copies bytes from src to dst, propagating EOF. Returns on the
// first read error or write error — the spliceBidi caller force-closes both
// streams once either direction returns, which unblocks the reverse relay.
func relayBytes(src, dst trsf.BidirectionalStream) {
	for {
		data, eof, err := src.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			if werr := dst.AppendData(eof, data); werr != nil {
				return
			}
		} else if eof {
			_ = dst.AppendData(true)
		}
		if eof {
			return
		}
	}
}

// handleList streams the snapshot (runners + tasks) over a server-initiated
// trsf send-stream. The TaskControlResponse{List} carries only the stream
// id; the actual ListResultBody is encoded onto the stream so the response
// fits in any path MTU on UDP.
//
// Body is written synchronously (full payload already in memory, no I/O
// like GetTaskLog has). On stream-creation failure (test stub or
// non-streaming connection) the response carries StreamId=0; client treats
// that as an error.
func (h *TaskHandler) handleList(conn ConnHandle, requestID uint32, connID string) {
	respond := func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_List, RequestId: requestID}
		resp.SetList(protocol.ListResult{StreamId: streamID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	all, allowed := h.visibleToCaller(connID)
	runners := h.Registry.List()
	tasks := h.Tasks.List(100)
	runnerInfos := make([]protocol.RunnerInfo, len(runners))
	for i, r := range runners {
		runnerInfos[i] = toRunnerInfo(r)
	}
	var filteredTasks []TaskEntry
	if all {
		filteredTasks = tasks
	} else {
		for _, t := range tasks {
			if allowed[t.ID] {
				filteredTasks = append(filteredTasks, t)
			}
		}
	}
	taskInfos := make([]protocol.TaskInfo, len(filteredTasks))
	for i, t := range filteredTasks {
		taskInfos[i] = toTaskInfo(t)
	}
	var body protocol.ListResultBody
	body.SetRunners(runnerInfos)
	body.SetTasks(taskInfos)

	bodyBytes, err := body.EncodeCopy(nil)
	if err != nil {
		slog.Error("List: encode body failed", "err", err)
		respond(0)
		return
	}

	stream := conn.CreateSendStream()
	if stream == nil {
		respond(0)
		return
	}
	if werr := stream.AppendData(false, bodyBytes); werr != nil {
		slog.Warn("List: stream write failed", "err", werr)
		respond(0)
		return
	}
	if werr := stream.AppendData(true); werr != nil {
		slog.Warn("List: stream EOF failed", "err", werr)
		respond(0)
		return
	}
	respond(uint64(stream.ID()))
}

// handleGetTaskLog responds to a GetTaskLog request by opening the per-task
// log file at <LogsDir>/<taskID>.log, allocating a server-initiated
// unidirectional stream, sending a TaskControlResponse referencing that
// stream's id, and then streaming the file content + EOF asynchronously.
//
// If LogsDir is empty (server started without --data-dir) or the file does
// not exist, the response carries Found=0 and StreamId=0.
func (h *TaskHandler) handleGetTaskLog(conn ConnHandle, requestID uint32, taskID string, connID string) {
	respond := func(found uint8, streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_GetTaskLog, RequestId: requestID}
		resp.SetGetLog(protocol.GetTaskLogResponse{Found: found, StreamId: streamID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	// Visibility gate: out-of-subtree tasks are indistinguishable from absent.
	visAll, allowed := h.visibleToCaller(connID)
	if !visAll && !allowed[taskID] {
		respond(0, 0)
		return
	}

	if h.LogsDir == "" {
		respond(0, 0)
		return
	}
	logPath := filepath.Join(h.LogsDir, taskID+".log")
	f, err := os.Open(logPath)
	if err != nil {
		// Includes os.ErrNotExist (no log yet) and any I/O error.
		respond(0, 0)
		return
	}
	stream := conn.CreateSendStream()
	if stream == nil {
		// Test or non-streaming connection: degrade to "no log".
		f.Close()
		respond(0, 0)
		return
	}

	respond(1, uint64(stream.ID()))

	// Stream the file content asynchronously so the response goes out first.
	go func() {
		defer f.Close()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if werr := stream.AppendData(false, buf[:n]); werr != nil {
					slog.Warn("GetTaskLog: stream write failed", "task_id", taskID, "err", werr)
					break
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				slog.Warn("GetTaskLog: file read failed", "task_id", taskID, "err", rerr)
				break
			}
		}
		// Signal EOF on the stream so the client knows we're done.
		if err := stream.AppendData(true); err != nil {
			slog.Warn("GetTaskLog: stream EOF failed", "task_id", taskID, "err", err)
		}
	}()
}

// toRunnerInfo converts a RunnerEntry value snapshot to the wire-format RunnerInfo.
// NOTE: RunnerInfo.Id is filled with an IPv4 placeholder; the real objproto.ConnectionID
// → protocol.RunnerID round-trip is deferred to a later task (CLI-side display will not show
// the runner identity precisely, but server-internal logic uses RunnerEntry.ID directly).
func toRunnerInfo(r RunnerEntry) protocol.RunnerInfo {
	info := protocol.RunnerInfo{
		Status:      r.Status(),
		MaxTasks:    uint16(r.MaxTasks),
		ConnectedAt: uint64(r.ConnectedAt.UnixNano()),
		LastSeen:    uint64(r.LastSeen.UnixNano()),
	}
	info.SetHostname([]byte(r.Hostname))
	info.SetAgentBin([]byte(r.AgentBin))
	info.SetSkillsInjected(r.SkillsInjected)
	info.Id = protocol.ConnIDToRunnerID(r.Conn.ConnectionID())

	// Populate AllowedRoots.
	roots := make([]protocol.AllowedRoot, len(r.AllowedRoots))
	for i, root := range r.AllowedRoots {
		var ar protocol.AllowedRoot
		ar.SetPath([]byte(root))
		roots[i] = ar
	}
	info.SetAllowedRoots(roots)

	// Populate ActiveTasks.
	activeTasks := make([]protocol.ActiveTaskRef, 0, len(r.ActiveTasks))
	for taskID := range r.ActiveTasks {
		var ref protocol.ActiveTaskRef
		raw, _ := hex.DecodeString(taskID)
		copy(ref.TaskId.Id[:], raw)
		activeTasks = append(activeTasks, ref)
	}
	info.ActiveTasksLen = uint16(len(activeTasks))
	info.ActiveTasks = activeTasks

	return info
}

// toTaskInfo converts a TaskEntry value snapshot to the wire-format TaskInfo.
// NOTE: TaskInfo.AssignedTo is filled with an IPv4 placeholder for the same reason
// as RunnerInfo.Id — the real round-trip is deferred (see comment in toRunnerInfo).
func toTaskInfo(t TaskEntry) protocol.TaskInfo {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(t.ID)
	copy(tid.Id[:], raw)

	info := protocol.TaskInfo{
		Id:            tid,
		Status:        t.Status,
		Kind:          t.Kind,
		OriginKind:    t.OriginKind,
		ResumedByKind: t.ResumedByKind,
		CreatorTaskId: t.CreatorTaskID,
		CreatedAt:     uint64(t.CreatedAt.UnixNano()),
	}
	info.SetRepoPath([]byte(t.RepoPath))
	info.SetWorktreeDir([]byte(t.WorktreeDir))
	info.SetPrompt([]byte(t.Prompt))
	info.SetErrorMessage([]byte(t.ErrorMsg))
	parsed, err := objproto.ParseConnectionID(t.AssignedTo, 0)
	if err == nil {
		info.AssignedTo = protocol.ConnIDToRunnerID(parsed)
	}

	if t.StartedAt != nil {
		info.StartedAt = uint64(t.StartedAt.UnixNano())
	}
	if t.EndedAt != nil {
		info.EndedAt = uint64(t.EndedAt.UnixNano())
	}
	if t.ExitCode != nil {
		info.ExitCode = *t.ExitCode
	}
	// Detach/reattach fields.
	info.SetDetachable(t.Detachable)
	info.SetIsAttached(t.IsAttached)
	info.RingBufferBytes = t.RingBufferBytes
	return info
}

// handleNotify runs both legs of a notify request: the live leg (OnNotify →
// ring + topic) and the egress leg (NotifyHook exec), then replies with the
// resulting NotifyStatus. accepted = hook launched; no_hook = egress disabled
// (live leg still ran); spawn_failed = hook failed to start.
func (h *TaskHandler) handleNotify(conn ConnHandle, req *protocol.TaskControlRequest) {
	nr := req.Notify()
	if nr == nil {
		slog.Error("TaskHandler: Notify variant is nil")
		return
	}
	cid := conn.ConnectionID().String()
	ck := h.lookupClientKind(cid)

	ts := time.Now().Unix()
	ev := protocol.NotifyEvent{
		Ts:         uint64(ts),
		ClientKind: ck,
		Level:      nr.Level,
		Origin:     nr.Origin,
		TitleLen:   nr.TitleLen,
		Title:      nr.Title,
		TextLen:    nr.TextLen,
		Text:       nr.Text,
	}
	if w := nr.Worker(); w != nil {
		ev.SetWorker(*w)
	}

	if h.OnNotify != nil {
		h.OnNotify(ev)
	}

	payload := notifyHookPayload{
		Level:  nr.Level.String(),
		Origin: nr.Origin.String(),
		Title:  string(nr.Title),
		Text:   string(nr.Text),
		ConnID: cid,
		Ts:     ts,
	}
	if w := nr.Worker(); w != nil {
		payload.TaskID = string(w.TaskId)
		payload.RunnerID = string(w.RunnerId)
		payload.Repo = string(w.Repo)
		payload.Hostname = string(w.Hostname)
	}
	status := runNotifyHook(h.NotifyHook, payload)

	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Notify, RequestId: req.RequestId}
	resp.SetNotify(protocol.NotifyResponse{Status: status})
	out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
	conn.SendMessage(out) //nolint:errcheck
}

// placeholderRunnerID returns a safe, encodable RunnerID with a loopback IPv4 address.
// The RunnerID encoder has a hard assertion that IpAddrLen == 4 || IpAddrLen == 16;
// encoding a zero-value RunnerID PANICS. Always use this function when building a
// RunnerInfo or TaskInfo that requires a RunnerID field.
func placeholderRunnerID() protocol.RunnerID {
	rid := protocol.RunnerID{
		Port:         0,
		UniqueNumber: 0,
	}
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{127, 0, 0, 1})
	return rid
}
