package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"path"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// RunnerHandler decodes inbound RunnerMessage payloads from runners
// and applies them to Registry and TaskStore.
type RunnerHandler struct {
	Registry      *Registry
	Tasks         *TaskStore
	Now           func() time.Time
	OnChange      func()              // called after any state mutation, used to trigger Scheduler.Tick
	OnTaskStarted func(taskID string) // optional; called when the runner reports TaskStarted

	// Board is the agentboard instance for ticket lifecycle management.
	// When nil, ticket revocation is skipped (safe for tests that do not wire a Board).
	Board *agentboard.Board

	// OnEstablishRelayResponse, when non-nil, is invoked with the runner's
	// stringified ConnectionID and the decoded EstablishRelayResponse. Server.New
	// wires this to Server.deliverEstablishRelayResponse, which routes the reply
	// to the goroutine that sent the corresponding EstablishRelayRequest.
	//
	// Nil-safe: tests that do not exercise the via-relay path leave it unwired.
	OnEstablishRelayResponse func(conn ConnHandle, resp protocol.EstablishRelayResponse)

	// TakePendingViaInfo, when non-nil, retrieves and removes the
	// ViaRegistrationInfo stashed by the OnDialed callback for the given
	// ConnectionID. Populated on Phase C (HandleWithVia) dials; nil return
	// means Phase A direct or reverse-dial — leave entry.Via + entry.ViaDialAddr zero.
	// Server.Run wires this to Server.takePendingViaInfo.
	TakePendingViaInfo func(cid objproto.ConnectionID) *ViaRegistrationInfo

	// ChainedRelay, when non-nil, handles RunnerMessage{RequestChainedRelay}
	// from runners that were registered via Phase C. Wired to
	// Server.chainedRelay in Server.New.
	// Nil-safe: tests that do not exercise the chained-relay path leave it unwired.
	ChainedRelay *ChainedRelayHandler

	// OnRemoteForwardConn, when non-nil, handles RunnerMessage{RemoteForwardConn}
	// (a connection accepted on a remote-forward listener). Wired to
	// TaskHandler.handleRemoteForwardConn in Server.New.
	OnRemoteForwardConn func(conn ConnHandle, msg *protocol.RemoteForwardConn)

	// OnRemoteForwardBindResult, when non-nil, handles
	// RunnerMessage{RemoteForwardBindResult} (the runner's listener-bind result).
	// Wired to TaskHandler.handleRemoteForwardBindResult in Server.New.
	OnRemoteForwardBindResult func(conn ConnHandle, msg *protocol.RemoteForwardBindResult)

	// OnConnIdentified, when non-nil, is called after a runner has been
	// registered via RunnerHello so the server can emit a conn_identified event
	// on conns.status. Called with the runner's connection ID string.
	// Nil-safe; tests that do not exercise the event path leave it unwired.
	OnConnIdentified func(cidStr string)
}

// Handle decodes a RunnerMessage payload (the full bytes including the Kind byte,
// as produced by RunnerMessage.MustAppend) and applies it to the Registry and TaskStore.
// Decode failures and missing inner variants are logged and silently dropped (no panic).
func (h *RunnerHandler) Handle(conn ConnHandle, payload []byte) {
	var msg protocol.RunnerMessage
	if err := msg.DecodeExact(payload); err != nil {
		slog.Error("RunnerHandler: failed to decode RunnerMessage", "error", err)
		return
	}

	runnerID := conn.ConnectionID().String()
	now := h.Now()

	switch msg.Kind {
	case protocol.RunnerMessageType_Hello:
		hello := msg.Hello()
		if hello == nil {
			slog.Error("RunnerHandler: Hello variant is nil", "runnerID", runnerID)
			return
		}
		maxTasks := int(hello.MaxTasks)
		if maxTasks < 1 {
			maxTasks = 1
		}
		roots := make([]string, len(hello.AllowedRoots))
		for i, ar := range hello.AllowedRoots {
			// Wire is POSIX '/'-paths; use path.Clean (not filepath.Clean) so a
			// Windows-running server doesn't convert '/' to '\' and break the
			// boundary predicate.
			roots[i] = path.Clean(string(ar.Path))
		}
		profiles := make([]string, len(hello.AgentProfiles))
		for i, p := range hello.AgentProfiles {
			profiles[i] = string(p.Name)
		}
		entry := &RunnerEntry{
			ID:             runnerID,
			Hostname:       string(hello.Hostname),
			AllowedRoots:   roots,
			MaxTasks:       maxTasks,
			AgentBin:       string(hello.AgentBin),
			AgentProfiles:  profiles,
			SkillsInjected: hello.SkillsInjected(),
			ActiveTasks:    make(map[string]struct{}),
			ConnectedAt:    now,
			LastSeen:       now,
		}
		entry.Conn = conn
		// Populate Via + ViaDialAddr from the pending info stashed by OnDialed
		// for Phase C (HandleWithVia) registrations. nil means Phase A direct or
		// reverse-dial — leave both fields at zero value.
		if h.TakePendingViaInfo != nil {
			if info := h.TakePendingViaInfo(conn.ConnectionID()); info != nil {
				entry.Via = info.Via
				entry.ViaDialAddr = info.ViaDialAddr
			}
		}
		h.Registry.Add(entry)

		// Notify the server that runner identity is now established so it can
		// emit a conn_identified event on conns.status.
		if h.OnConnIdentified != nil {
			h.OnConnIdentified(runnerID)
		}

		// Tell the runner what canonical RunnerID the server keys it as.
		// The peer transport's ConnectionID is symmetric (surfaces the peer's
		// ID), so the runner cannot derive this locally; without this the
		// runner would inject the wrong HARNESS_RUNNER_ID and agent Hello
		// validation would fail.
		rhResp := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_RunnerHelloResponse}
		rhResp.SetRunnerHelloResponse(protocol.RunnerHelloResponse{
			YourRunnerId: runnerIDFromConnID(runnerID),
		})
		if rhBytes, err := rhResp.Append([]byte{byte(appwire.AppKind_RunnerControl)}); err != nil {
			slog.Error("RunnerHandler: encode RunnerHelloResponse failed", "runner", runnerID, "err", err)
		} else if _, _, err := conn.SendMessage(rhBytes); err != nil {
			slog.Error("RunnerHandler: send RunnerHelloResponse failed", "runner", runnerID, "err", err)
		}

	case protocol.RunnerMessageType_TaskAccepted:
		ta := msg.TaskAccepted()
		if ta == nil {
			slog.Error("runner_handler: TaskAccepted variant missing", "runner", runnerID)
			return
		}
		accepted := hex.EncodeToString(ta.TaskId.Id[:])
		if e, ok := h.Registry.Get(runnerID); ok {
			if _, has := e.ActiveTasks[accepted]; !has && len(e.ActiveTasks) > 0 {
				slog.Warn("runner_handler: runner accepted task not in ActiveTasks",
					"runner", runnerID, "accepted", accepted)
			}
		}
		if !h.Registry.SetLastSeen(runnerID, now) {
			slog.Error("runner_handler: SetLastSeen on unknown runner", "runner", runnerID)
			return
		}

	case protocol.RunnerMessageType_TaskStarted:
		taskStarted := msg.TaskStarted()
		if taskStarted == nil {
			slog.Error("RunnerHandler: TaskStarted variant is nil", "runnerID", runnerID)
			return
		}
		taskID := hex.EncodeToString(taskStarted.TaskId.Id[:])
		if !h.Tasks.SetWorktreeDir(taskID, string(taskStarted.WorktreeDir)) {
			slog.Error("RunnerHandler: TaskStarted for unknown task", "runnerID", runnerID, "taskID", taskID)
			return
		}
		if h.OnTaskStarted != nil {
			h.OnTaskStarted(taskID)
		}

	case protocol.RunnerMessageType_TaskFinished:
		tf := msg.TaskFinished()
		if tf == nil {
			slog.Error("RunnerHandler: TaskFinished variant is nil", "runnerID", runnerID)
			return
		}
		taskID := hex.EncodeToString(tf.TaskId.Id[:])
		// Tasks.Finish silently no-ops if task is not found — that is acceptable.
		h.Tasks.Finish(taskID, tf.ExitCode, tf.ErrorMessage)
		// Release the capacity slot so the dispatcher can re-use it.
		h.Registry.UnbindTask(runnerID, taskID)
		// Revoke the auth ticket so the agent can no longer authenticate for this task.
		if h.Board != nil {
			h.Board.Revoke(runnerIDFromConnID(runnerID), taskIDFromHex(taskID))
		}

	case protocol.RunnerMessageType_Heartbeat:
		if !h.Registry.SetLastSeen(runnerID, now) {
			slog.Error("RunnerHandler: Heartbeat from unknown runner", "runnerID", runnerID)
			return
		}

	case protocol.RunnerMessageType_EstablishRelayResponse:
		er := msg.EstablishRelayResponse()
		if er == nil {
			slog.Error("RunnerHandler: EstablishRelayResponse variant is nil", "runnerID", runnerID)
			return
		}
		if h.OnEstablishRelayResponse != nil {
			h.OnEstablishRelayResponse(conn, *er)
		} else {
			slog.Warn("RunnerHandler: EstablishRelayResponse arrived but no handler wired",
				"runnerID", runnerID, "status", er.Status)
		}
		// EstablishRelayResponse does not mutate Registry/Tasks; suppress the
		// trailing OnChange so we don't run a spurious Scheduler.Tick.
		return

	case protocol.RunnerMessageType_RequestChainedRelay:
		rcr := msg.RequestChainedRelay()
		if rcr == nil {
			slog.Error("RunnerHandler: RequestChainedRelay variant is nil", "runnerID", runnerID)
			return
		}
		if h.ChainedRelay == nil {
			slog.Warn("RunnerHandler: RequestChainedRelay arrived but no handler wired",
				"runnerID", runnerID)
			return
		}
		resp := h.ChainedRelay.Handle(context.Background(), conn, *rcr)
		rrResp := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ChainedRelayResponse}
		rrResp.SetChainedRelayResponse(resp)
		if rrBytes, err := rrResp.Append([]byte{byte(appwire.AppKind_RunnerControl)}); err != nil {
			slog.Error("RunnerHandler: encode ChainedRelayResponse failed", "runner", runnerID, "err", err)
		} else if _, _, err := conn.SendMessage(rrBytes); err != nil {
			slog.Error("RunnerHandler: send ChainedRelayResponse failed", "runner", runnerID, "err", err)
		}
		// RequestChainedRelay does not mutate Registry/Tasks; suppress the
		// trailing OnChange so we don't run a spurious Scheduler.Tick.
		return

	case protocol.RunnerMessageType_RemoteForwardConn:
		rfc := msg.RemoteForwardConn()
		if rfc == nil {
			slog.Error("RunnerHandler: RemoteForwardConn variant is nil", "runnerID", runnerID)
			return
		}
		if h.OnRemoteForwardConn != nil {
			// Run async: handleRemoteForwardConn blocks on WaitForBidirectionalStream
			// for the runner-created data stream, and this dispatch runs on the
			// connection's recv goroutine — blocking it would stall the very
			// stream-frame demux we are waiting on (self-deadlock until timeout).
			go h.OnRemoteForwardConn(conn, rfc)
		}
		// Does not mutate Registry/Tasks; suppress the trailing OnChange.
		return

	case protocol.RunnerMessageType_RemoteForwardBindResult:
		br := msg.RemoteForwardBindResult()
		if br == nil {
			slog.Error("RunnerHandler: RemoteForwardBindResult variant is nil", "runnerID", runnerID)
			return
		}
		if h.OnRemoteForwardBindResult != nil {
			// Non-blocking (signals a buffered channel); safe to run inline.
			h.OnRemoteForwardBindResult(conn, br)
		}
		return

	default:
		slog.Error("RunnerHandler: unhandled message kind", "runnerID", runnerID, "kind", msg.Kind)
		return
	}

	if h.OnChange != nil {
		h.OnChange()
	}
}
