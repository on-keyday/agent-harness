package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// SetParentOpts describes one parent-link change. Exactly one of the three
// forms is meant per call: ParentID set (re-point), ParentID empty with
// Swap=false (detach to the operator root), or Swap=true (invert with the
// current parent). ParentID+Swap together is rejected client-side before it
// can reach the wire's swap_takes_no_parent answer.
type SetParentOpts struct {
	TaskID   string // hex (full 32)
	ParentID string // hex; "" = detach to root (all-zero on the wire)
	Swap     bool
}

// SetParentResult is the decoded server answer. Hex ids; "" = the root.
type SetParentResult struct {
	Status    protocol.SetParentStatus
	OldParent string
	NewParent string
	SwappedID string // swap only: the former parent, now the target's child
}

// SetParentWith re-points a live task's parent link over an already-connected
// client. Operator-only, enforced server-side by the caller having no
// principal task. Long-lived embedders (TUI/WebUI) call this form.
func SetParentWith(ctx context.Context, c taskControlClient, opts SetParentOpts) (SetParentResult, error) {
	if opts.Swap && opts.ParentID != "" {
		return SetParentResult{}, fmt.Errorf("set-parent: --swap picks the task's CURRENT parent; it cannot be combined with --parent")
	}
	tid, err := parseTaskIDHex(opts.TaskID)
	if err != nil {
		return SetParentResult{}, fmt.Errorf("set-parent: %w", err)
	}
	body := protocol.SetParentRequest{TaskId: tid}
	if opts.ParentID != "" {
		pid, err := parseTaskIDHex(opts.ParentID)
		if err != nil {
			return SetParentResult{}, fmt.Errorf("set-parent: parent: %w", err)
		}
		body.ParentId = pid
	}
	body.SetSwap(opts.Swap)

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_SetParent}
	req.SetSetParent(body)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return SetParentResult{}, err
	}
	sp := resp.SetParent()
	if resp.Kind != protocol.TaskControlKind_SetParent || sp == nil {
		return SetParentResult{}, fmt.Errorf("unexpected response: %+v", resp)
	}
	hexOrEmpty := func(id protocol.TaskID) string {
		if id.Id == ([16]byte{}) {
			return ""
		}
		return hex.EncodeToString(id.Id[:])
	}
	out := SetParentResult{
		Status:    sp.Status,
		OldParent: hexOrEmpty(sp.OldParent),
		NewParent: hexOrEmpty(sp.NewParent),
		SwappedID: hexOrEmpty(sp.SwappedId),
	}
	return out, setParentStatusError(sp.Status)
}

// SetParent dials the server, applies the change and closes. Short-lived
// callers (the harness-cli binary) use this form.
func SetParent(ctx context.Context, serverCID objproto.ConnectionID, opts SetParentOpts) (SetParentResult, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return SetParentResult{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return SetParentWith(ctx, c, opts)
}

// SetParentMessage renders the operator-facing result line: it names the
// target and the change, never a bare count. Shared by the CLI and TUI; the
// WebUI builds the same shapes in JS beside its other cmd-output messages.
func SetParentMessage(opts SetParentOpts, res SetParentResult) string {
	short := func(hexID string) string {
		if hexID == "" {
			return "(root)"
		}
		return hexID[:8]
	}
	t := short(opts.TaskID)
	if opts.Swap {
		return fmt.Sprintf("set-parent %s --swap: %s now under %s, %s now under %s",
			t, t, short(res.NewParent), short(res.SwappedID), t)
	}
	return fmt.Sprintf("set-parent %s: parent=%s → %s", t, short(res.OldParent), short(res.NewParent))
}

func setParentStatusError(s protocol.SetParentStatus) error {
	switch s {
	case protocol.SetParentStatus_Ok:
		return nil
	case protocol.SetParentStatus_NotFound:
		return fmt.Errorf("no such task")
	case protocol.SetParentStatus_ParentNotFound:
		return fmt.Errorf("no such parent task (or the current parent's record is gone)")
	case protocol.SetParentStatus_WouldCycle:
		return fmt.Errorf("would cycle: the requested parent is the task itself or one of its own descendants")
	case protocol.SetParentStatus_NoParent:
		return fmt.Errorf("no parent to swap with: the task is already operator-rooted")
	case protocol.SetParentStatus_SwapTakesNoParent:
		return fmt.Errorf("swap takes no parent id: --swap picks the task's current parent")
	case protocol.SetParentStatus_NotOperator:
		return fmt.Errorf("operator only: the parent link can be re-pointed from an " +
			"operator connection, not from inside a task")
	default:
		return fmt.Errorf("set-parent failed: %v", s)
	}
}
