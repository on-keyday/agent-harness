package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// checkPrompt refuses a prompt the server would refuse (protocol.PromptMax,
// pillar A spec A-D17) before anything is sent.
func checkPrompt(prompt string) error {
	if len(prompt) > protocol.PromptMax {
		return fmt.Errorf("prompt is %d bytes; the most a session's prompt may hold is %d", len(prompt), protocol.PromptMax)
	}
	return nil
}

// roundTripOpenRequest sends an open-interactive request. With a prompt, the
// prompt rides its own payload stream written after the request (A-D17), so a
// request of any prompt size still fits one datagram.
func (c *Client) roundTripOpenRequest(ctx context.Context, oi protocol.OpenInteractiveRequest, prompt string) (*protocol.TaskControlResponse, error) {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive}}
	if prompt == "" {
		req.SetOpenInteractive(oi)
		return c.RoundTripTaskControl(ctx, req)
	}
	if err := checkPrompt(prompt); err != nil {
		return nil, err
	}
	body := protocol.PromptBody{}
	body.SetPrompt([]byte(prompt))
	enc, err := body.EncodeCopy(nil)
	if err != nil {
		return nil, fmt.Errorf("encode prompt: %w", err)
	}
	res, err := c.TaskControlWithPayload(ctx, func(streamID uint64) (*protocol.TaskControlRequest, error) {
		oi.SetPromptPresent(true)
		oi.SetPromptStreamId(streamID)
		req.SetOpenInteractive(oi)
		return req, nil
	}, enc)
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	return res.Resp, nil
}

// runModeStatusError is the text of the open statuses pillar A added; nil for
// any other status, which the caller's own switch answers.
func runModeStatusError(status protocol.OpenInteractiveStatus) error {
	switch status {
	case protocol.OpenInteractiveStatus_DeadlineRequired:
		return fmt.Errorf("interactive deadline_required: a background session must state its deadline (--deadline)")
	case protocol.OpenInteractiveStatus_ProfileRequired:
		return fmt.Errorf("interactive profile_required: a new background session must name its agent (--agent)")
	case protocol.OpenInteractiveStatus_PromptRequired:
		return fmt.Errorf("interactive prompt_required: a background session must carry a prompt (--prompt)")
	case protocol.OpenInteractiveStatus_TurnEndUnsupported:
		return fmt.Errorf("interactive turn_end_unsupported: a stream session reports lifecycle only; use --turn-end lifecycle, any or off")
	case protocol.OpenInteractiveStatus_PromptTooLarge:
		return fmt.Errorf("interactive prompt_too_large: the most a session's prompt may hold is %d bytes", protocol.PromptMax)
	case protocol.OpenInteractiveStatus_BadPrompt:
		return fmt.Errorf("interactive bad_prompt: the prompt stream could not be read")
	case protocol.OpenInteractiveStatus_ConditionsWithoutBackground:
		return fmt.Errorf("interactive conditions_without_background: --deadline/--turn-end/--quiet/--blocked/--queue-timeout belong to a background session; add --background")
	}
	return nil
}

// sessionModeStatusError turns a session_mode answer into an error; nil for ok.
// The server's message, when it gives one, is the answer.
func sessionModeStatusError(status protocol.SessionModeStatus, serverMsg []byte) error {
	if status == protocol.SessionModeStatus_Ok {
		return nil
	}
	if len(serverMsg) != 0 {
		return fmt.Errorf("session mode %s: %s", EnumWireName(status.String()), serverMsg)
	}
	switch status {
	case protocol.SessionModeStatus_NotFound:
		return fmt.Errorf("session mode not_found: no such task, or it is outside your cancel scope")
	case protocol.SessionModeStatus_NotSession:
		return fmt.Errorf("session mode not_session: the task is a oneshot, or not running")
	case protocol.SessionModeStatus_Queued:
		return fmt.Errorf("session mode queued: the task is queued; cancel it and resume it in the other mode")
	case protocol.SessionModeStatus_DeadlineRequired:
		return fmt.Errorf("session mode deadline_required: a switch to background must state its deadline (--deadline)")
	case protocol.SessionModeStatus_TurnEndUnsupported:
		return fmt.Errorf("session mode turn_end_unsupported: a stream session reports lifecycle only")
	case protocol.SessionModeStatus_SelfForegroundRefused:
		return fmt.Errorf("session mode self_foreground_refused: a task may not switch itself to foreground")
	case protocol.SessionModeStatus_InternalError:
		return fmt.Errorf("session mode internal_error")
	}
	return fmt.Errorf("session mode error (status=%d)", status)
}

// SessionMode switches a running session task between foreground and
// background (pillar A spec, A-D19). A switch to background needs cond with a
// deadline.
func (c *Client) SessionMode(ctx context.Context, taskIDHex string, background bool, cond *protocol.BackgroundConditions) error {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return err
	}
	sm := protocol.SessionModeRequest{TaskId: tid}
	sm.SetBackground(background)
	if cond != nil {
		sm.SetConditionsPresent(true)
		sm.SetConditions(*cond)
	}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SessionMode}}
	req.SetSessionMode(sm)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	r := resp.SessionMode()
	if resp.Kind != protocol.TaskControlKind_SessionMode || r == nil {
		return fmt.Errorf("expected a session_mode response, got kind=%v", resp.Kind)
	}
	return sessionModeStatusError(r.Status, r.ErrorMsg)
}

// RunStr is a task's run-mode facts as one `ls` fragment (A-D14), "" when
// there is nothing to say: run=bg for a background task; while it is queued,
// why it waits (placing = a runner can take it and Tick is about to); while it
// runs, the time left to its deadline (server clock); once ended, why — every
// task, both modes — and, for a cancel, who did it. A terminal task from
// before end reasons existed reads end=unknown. Shared by the CLI and the TUI.
func RunStr(t *protocol.TaskInfo) string {
	var parts []string
	bg := t.RunMode == protocol.RunMode_Background
	if bg {
		parts = append(parts, "run=bg")
	}
	switch {
	case bg && t.Status == protocol.TaskStatus_Queued:
		w := "placing"
		switch t.WaitReason {
		case protocol.WaitReason_NoCandidate:
			w = "no_candidate"
		case protocol.WaitReason_AllBusy:
			w = "all_busy"
		}
		parts = append(parts, "wait="+w)
	case bg && t.EndedAt == 0 && t.DeadlineNs != 0:
		left := (time.Duration(t.DeadlineRemainingMs) * time.Millisecond).Round(time.Second)
		parts = append(parts, "deadline="+left.String())
	}
	if t.EndedAt != 0 {
		r := "unknown"
		if t.EndReason != protocol.EndReason_None {
			r = EnumWireName(t.EndReason.String())
		}
		parts = append(parts, "end="+r)
		if t.EndReason == protocol.EndReason_Cancelled {
			switch {
			case t.CancelledByHidden():
				parts = append(parts, "by=hidden")
			case t.CancelledBy.Id == ([16]byte{}):
				parts = append(parts, "by=operator")
			default:
				parts = append(parts, "by="+hex.EncodeToString(t.CancelledBy.Id[:4]))
			}
		}
	}
	return strings.Join(parts, " ")
}

// EnumWireName turns a generated enum's Go name (TurnDoneQuiet) into the
// schema's name (turn_done_quiet), the form operators read and type.
func EnumWireName(goName string) string {
	var b strings.Builder
	for i, r := range goName {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// RunModeFlags are a session open's run-mode inputs as a front-end collects
// them (pillar A spec A-D15). Apply writes them onto SessionOpts.
type RunModeFlags struct {
	Background, Foreground                 bool
	Prompt                                 string
	Deadline, Quiet, Blocked, QueueTimeout time.Duration
	TurnEnd                                protocol.TurnEndSignal
}

// Conditions is the background conditions block the flags describe, nil when
// they name none. The server refuses a block on an open that resolves to
// foreground, so a stray --deadline is caught rather than dropped.
func (f RunModeFlags) Conditions() *protocol.BackgroundConditions {
	if f.Deadline == 0 && f.Quiet == 0 && f.Blocked == 0 && f.QueueTimeout == 0 && f.TurnEnd == protocol.TurnEndSignal_Unset {
		return nil
	}
	return &protocol.BackgroundConditions{
		DeadlineMs:     uint64(f.Deadline.Milliseconds()),
		QuietMs:        uint64(f.Quiet.Milliseconds()),
		BlockedMs:      uint64(f.Blocked.Milliseconds()),
		QueueTimeoutMs: uint64(f.QueueTimeout.Milliseconds()),
		TurnEnd:        f.TurnEnd,
	}
}

// Apply sets the run mode, the conditions and the prompt on o. A background
// open — named, or a resume carrying conditions — is detached: it takes no
// seat, and its size rides only the detached block.
func (f RunModeFlags) Apply(o *SessionOpts) {
	o.Prompt = f.Prompt
	if f.Background || f.Foreground {
		o.RunModeSet = true
		o.Background = f.Background
	}
	o.Conditions = f.Conditions()
	if f.Background || o.Conditions != nil {
		o.Detached = true
	}
}
