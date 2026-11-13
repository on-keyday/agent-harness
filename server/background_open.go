package server

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// resolveRunMode is the run mode an open asks for (A-D1): the one it names,
// else the task's own on a resume, else foreground.
func (h *TaskHandler) resolveRunMode(req *protocol.OpenInteractiveRequest, resumeHex string) protocol.RunMode {
	if req.RunModeSet() {
		if req.Background() {
			return protocol.RunMode_Background
		}
		return protocol.RunMode_Foreground
	}
	if resumeHex != "" {
		if t, ok := h.Tasks.Get(resumeHex); ok {
			return t.RunMode
		}
	}
	return protocol.RunMode_Foreground
}

// backgroundOpen is what openBackground needs from handleOpenInteractive once
// the grant, the resume target and the repo are settled.
type backgroundOpen struct {
	cid       string
	req       *protocol.OpenInteractiveRequest
	origin    protocol.ClientKind
	creator   protocol.TaskID
	grant     GrantUpdate
	resumeHex string // "" = a create
	exactRec  *TaskEntry
	repo      string
	args      []string
	prompt    string
}

// openBackground queues a background session task (A-D4): it never chooses a
// runner or takes a slot, so it is never refused for capacity or for no
// runner. Tick places it (A-D12). The refusals that remain are what it cannot
// run without (A-D5, A-D6, A-D7), a turn-end signal its kind cannot report
// (F19), and — only when candidates exist to say so — a profile none offers.
func (h *TaskHandler) openBackground(b backgroundOpen) protocol.OpenInteractiveResponse {
	req := b.req
	refuse := func(st protocol.OpenInteractiveStatus, msg string) protocol.OpenInteractiveResponse {
		r := protocol.OpenInteractiveResponse{Status: st}
		if msg != "" {
			r.SetErrorMsg([]byte(msg))
		}
		return r
	}
	// A resume of a task still running answers that first, before anything
	// about the request's own fields.
	if b.resumeHex != "" {
		if t, ok := h.Tasks.Get(b.resumeHex); ok {
			switch t.Status {
			case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
			default:
				return refuse(protocol.OpenInteractiveStatus_ResumeNotTerminal, "")
			}
		}
	}
	// A background run takes no seat; its size travels only on a detached
	// open (OpenInteractiveRequest's detached block).
	if !req.Detached() {
		return refuse(protocol.OpenInteractiveStatus_DetachRequired, "a background session opens detached")
	}
	if req.X11Enabled() {
		return refuse(protocol.OpenInteractiveStatus_InternalError,
			"x11 forwarding needs a live client to host the tunnel, and a background session has none")
	}
	kind := interactiveKind(req)
	profile := string(req.AgentProfile)
	if b.exactRec != nil {
		profile = b.exactRec.AgentProfile
	}
	if profile == "" && b.resumeHex != "" {
		if t, ok := h.Tasks.Get(b.resumeHex); ok {
			profile = t.AgentProfile // which agent's conversation the task holds (A-D5)
		}
	}
	if profile == "" {
		return refuse(protocol.OpenInteractiveStatus_ProfileRequired, "a background open must name its agent")
	}
	wc := req.Conditions()
	if !req.ConditionsPresent() || wc == nil || wc.DeadlineMs == 0 {
		return refuse(protocol.OpenInteractiveStatus_DeadlineRequired, "a background open must state its deadline")
	}
	if b.prompt == "" {
		return refuse(protocol.OpenInteractiveStatus_PromptRequired, "a background open must carry a prompt")
	}
	switch p, msg := checkConditions(wc, kind); p {
	case condBadDuration:
		return refuse(protocol.OpenInteractiveStatus_DeadlineRequired, msg)
	case condBadTurnEnd:
		return refuse(protocol.OpenInteractiveStatus_TurnEndUnsupported, msg)
	}
	if cands := h.Registry.Candidates(b.repo, req.Selector); len(cands) > 0 {
		offered := false
		for _, c := range cands {
			if c.HasProfile(profile) {
				offered = true
				break
			}
		}
		if !offered {
			return refuse(protocol.OpenInteractiveStatus_ProfileUnavailable, "")
		}
	}

	cond := ConditionsFromWire(wc).WithDefaults(kind)
	if r, c := req.InitialRows(), req.InitialCols(); r != nil && c != nil {
		cond.Rows, cond.Cols = *r, *c
	}
	run := RunSpec{Mode: protocol.RunMode_Background, Cond: cond}

	var taskIDHex string
	if b.resumeHex != "" {
		if _, err := h.Tasks.Resume(b.resumeHex, b.prompt, b.args, req.Selector, "", b.origin, b.grant, kind, profile,
			h.lookupPrincipal(b.cid), req.ResumeConversation(), run); err != nil {
			switch {
			case errors.Is(err, ResumeErrNotFound):
				return refuse(protocol.OpenInteractiveStatus_ResumeNotFound, "")
			case errors.Is(err, ResumeErrNotTerminal):
				return refuse(protocol.OpenInteractiveStatus_ResumeNotTerminal, "")
			default:
				return refuse(protocol.OpenInteractiveStatus_InternalError, "")
			}
		}
		taskIDHex = b.resumeHex
	} else {
		created, err := h.Tasks.CreateWithinLimits(b.repo, b.prompt, kind, b.origin, b.creator, "", req.Selector,
			req.ExtraArgs.AsStrings(), b.grant.To, profile, run)
		if err != nil {
			return refuse(protocol.OpenInteractiveStatus_SpawnLimit, err.Error())
		}
		taskIDHex = created
		h.Tasks.SetResumeConversation(taskIDHex, req.ResumeConversation())
	}
	if h.OnChange != nil {
		// Tick places it now if a runner can take it — after the answer, not
		// before it (the opener is told queued, which it was).
		go h.OnChange()
	}
	resp := protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_Ok}
	raw, _ := hex.DecodeString(taskIDHex)
	copy(resp.TaskId.Id[:], raw)
	resp.SetQueued(true)
	return resp
}

// readOpenPrompt reads an open's prompt from the client's payload stream
// (A-D17), capped at protocol.PromptMax. On failure it returns the status and
// message to answer with; the stream is cancelled on an oversize body.
func readOpenPrompt(conn ConnHandle, id uint64) (string, protocol.OpenInteractiveStatus, string) {
	raw, err := readAgentPayloadStream(conn, id, protocol.PromptMax+4) // +4: PromptBody's length prefix
	if errors.Is(err, errPayloadTooLarge) {
		return "", protocol.OpenInteractiveStatus_PromptTooLarge, fmt.Sprintf("prompt exceeds %d bytes", protocol.PromptMax)
	}
	if err != nil {
		return "", protocol.OpenInteractiveStatus_BadPrompt, "prompt stream: " + err.Error()
	}
	var body protocol.PromptBody
	if err := body.DecodeExact(raw); err != nil {
		return "", protocol.OpenInteractiveStatus_BadPrompt, "prompt body: " + err.Error()
	}
	if len(body.Prompt) > protocol.PromptMax {
		return "", protocol.OpenInteractiveStatus_PromptTooLarge, fmt.Sprintf("prompt exceeds %d bytes", protocol.PromptMax)
	}
	return string(body.Prompt), protocol.OpenInteractiveStatus_Ok, ""
}

// waitReasonFor is why a queued background task waits (A-D14), computed when
// a listing is built from the same Candidates call Tick uses: no connected
// runner has the repo as a root and matches the selector and the profile, or
// such runners exist and none has a free slot. None for anything else — and
// for a queued task a runner could take now, which Tick is about to place.
func (h *TaskHandler) waitReasonFor(t TaskEntry) protocol.WaitReason {
	if t.Status != protocol.TaskStatus_Queued || t.RunMode != protocol.RunMode_Background || h.Registry == nil {
		return protocol.WaitReason_None
	}
	matching := 0
	for _, c := range h.Registry.Candidates(t.RepoPath, t.Selector) {
		if t.AgentProfile != "" && !c.HasProfile(t.AgentProfile) {
			continue
		}
		matching++
		if c.Status() == protocol.RunnerStatus_Idle && !h.Registry.displaced(c.ID) {
			return protocol.WaitReason_None
		}
	}
	if matching == 0 {
		return protocol.WaitReason_NoCandidate
	}
	return protocol.WaitReason_AllBusy
}

type condProblem int

const (
	condOK condProblem = iota
	condBadDuration
	condBadTurnEnd
)

// maxConditionMs bounds every *_ms condition: 100 years, far above any real
// run and far below where milliseconds overflow time.Duration (~292 years) —
// past it a deadline wraps to zero (no deadline at all) or to microseconds.
const maxConditionMs = 100 * 365 * 24 * 3600 * 1000

// checkConditions refuses a conditions block the server cannot honour: a
// duration out of range, a turn-end value the schema does not define, or a
// leg the kind cannot report (F19).
func checkConditions(wc *protocol.BackgroundConditions, kind protocol.TaskKind) (condProblem, string) {
	for name, v := range map[string]uint64{"deadline_ms": wc.DeadlineMs, "quiet_ms": wc.QuietMs, "blocked_ms": wc.BlockedMs, "queue_timeout_ms": wc.QueueTimeoutMs} {
		if v > maxConditionMs {
			return condBadDuration, fmt.Sprintf("%s %d is out of range (at most %d)", name, v, uint64(maxConditionMs))
		}
	}
	if wc.TurnEnd > protocol.TurnEndSignal_Any {
		return condBadTurnEnd, fmt.Sprintf("turn end %d is not defined", wc.TurnEnd)
	}
	if kind == protocol.TaskKind_Stream && (wc.TurnEnd == protocol.TurnEndSignal_Quiet || wc.TurnEnd == protocol.TurnEndSignal_Program) {
		return condBadTurnEnd, fmt.Sprintf("turn end %v is not available on a stream session: it reports lifecycle only", wc.TurnEnd)
	}
	return condOK, ""
}
