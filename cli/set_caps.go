package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// SetCapsOpts describes one live re-grant. Caps and Scope are pointers because
// omitting either means "keep what the task has": Capability(0) is a real
// value ("none") and TaskScope{} is a real scope ("subtree"), so neither field
// has a spare value to mean unset. The wire carries the same distinction as
// caps_present / scope_present bits.
type SetCapsOpts struct {
	TaskID    string // hex
	Caps      *protocol.Capability
	Scope     *protocol.TaskScope
	Cascade   bool
	KeepConns bool
}

// CapsPtr wraps a Capability for SetCapsOpts.Caps, whose pointer still encodes
// "the operator did not name this half of the authority" — unlike a spawn,
// where an omitted --caps means Capability_None and needs no pointer (see
// SessionOpts).
func CapsPtr(c protocol.Capability) *protocol.Capability { return &c }

// SetCapsResult is the decoded server answer.
type SetCapsResult struct {
	Status      protocol.SetCapsStatus
	Affected    []string // task-id hex: the target plus every descendant changed
	ConnsClosed uint32
}

// SetCapsWith rewrites a live task's authority over an already-connected
// client. Operator-only, enforced server-side by the caller having no
// principal task — an in-task agent gets SetCapsStatus_NotOperator however it
// asks. Long-lived embedders (TUI/WebUI) call this form.
func SetCapsWith(ctx context.Context, c taskControlClient, opts SetCapsOpts) (SetCapsResult, error) {
	if opts.Caps == nil && opts.Scope == nil {
		return SetCapsResult{}, fmt.Errorf("set-caps: nothing to change (pass --caps, --scope, or both)")
	}
	tid, err := parseTaskIDHex(opts.TaskID)
	if err != nil {
		return SetCapsResult{}, fmt.Errorf("set-caps: %w", err)
	}

	body := protocol.SetCapsRequest{TaskId: tid}
	if opts.Caps != nil {
		body.Caps = *opts.Caps
		body.SetCapsPresent(true)
	}
	if opts.Scope != nil {
		body.Scope = *opts.Scope
		body.SetScopePresent(true)
	}
	body.SetCascade(opts.Cascade)
	body.SetKeepConns(opts.KeepConns)

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_SetCaps}
	req.SetSetCaps(body)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return SetCapsResult{}, err
	}
	sc := resp.SetCaps()
	if resp.Kind != protocol.TaskControlKind_SetCaps || sc == nil {
		return SetCapsResult{}, fmt.Errorf("unexpected response: %+v", resp)
	}
	out := SetCapsResult{Status: sc.Status, ConnsClosed: sc.ConnsClosed}
	for _, id := range sc.Affected {
		out.Affected = append(out.Affected, hex.EncodeToString(id.Id[:]))
	}
	return out, setCapsStatusError(sc.Status)
}

// SetCaps dials the server, applies the re-grant and closes. Short-lived
// callers (the harness-cli binary) use this form.
func SetCaps(ctx context.Context, serverCID objproto.ConnectionID, opts SetCapsOpts) (SetCapsResult, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return SetCapsResult{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return SetCapsWith(ctx, c, opts)
}

func setCapsStatusError(s protocol.SetCapsStatus) error {
	switch s {
	case protocol.SetCapsStatus_Ok:
		return nil
	case protocol.SetCapsStatus_NotFound:
		return fmt.Errorf("no such task")
	case protocol.SetCapsStatus_NotOperator:
		return fmt.Errorf("operator only: capabilities can be re-granted from an " +
			"operator connection, not from inside a task")
	default:
		return fmt.Errorf("set-caps failed: %v", s)
	}
}
