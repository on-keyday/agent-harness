package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// Snapshot queries the server for all runners + recent tasks and returns the
// decoded ListResultBody. The wire response carries only a stream id; the
// body is read from the trsf send-stream the server opens (so the payload
// fits within UDP path MTU regardless of how many tasks the server holds).
//
// Both the human-readable List and the TUI/webui code paths share this
// helper so the RoundTripTaskControl + stream-decode logic exists in exactly
// one place.
func (c *Client) Snapshot(ctx context.Context) (*protocol.ListResultBody, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{Query: nil})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.List()
	if lr == nil {
		return nil, fmt.Errorf("expected List response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, fmt.Errorf("server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("list stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("list stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			break
		}
	}
	body := &protocol.ListResultBody{}
	if err := body.DecodeExact(raw); err != nil {
		return nil, fmt.Errorf("decode ListResultBody (%d bytes): %w", len(raw), err)
	}
	return body, nil
}

// List queries the server for all runners + recent tasks and writes a human-
// readable summary to out. Method form: callable repeatedly without re-dialing.
func (c *Client) List(ctx context.Context, out io.Writer) error {
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	renderList(lr, out)
	return nil
}

// renderList writes a human-readable summary of a ListResult to out.
// Extracted for testability: tests can construct a ListResult directly without
// a live server and call renderList to verify the rendered columns.
func renderList(lr *protocol.ListResultBody, out io.Writer) {
	fmt.Fprintln(out, "RUNNERS")
	if len(lr.Runners) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, r := range lr.Runners {
		roots := make([]string, len(r.AllowedRoots))
		for i, ar := range r.AllowedRoots {
			roots[i] = string(ar.Path)
		}
		fmt.Fprintf(out, "  %s  host=%s  tasks=%d/%d  %s  roots=%s  id=%s\n",
			runnerStatusStr(r.Status),
			string(r.Hostname),
			len(r.ActiveTasks),
			r.MaxTasks,
			agentStr(string(r.AgentBin), r.SkillsInjected()),
			strings.Join(roots, ","),
			protocol.RunnerIDToConnID(r.Id).String(),
		)
	}

	// Index runners by ConnID string so each task can show its runner's agent.
	runnerByID := make(map[string]protocol.RunnerInfo, len(lr.Runners))
	for _, r := range lr.Runners {
		runnerByID[protocol.RunnerIDToConnID(r.Id).String()] = r
	}
	fmt.Fprintln(out, "TASKS")
	if len(lr.Tasks) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, t := range lr.Tasks {
		agent := ""
		if r, ok := runnerByID[protocol.RunnerIDToConnID(t.AssignedTo).String()]; ok {
			agent = "  " + agentStr(string(r.AgentBin), r.SkillsInjected())
		}
		// exit= / err= render only when meaningful so the common rows stay
		// short: exit= for a finished task with a non-zero code, err= for a
		// server- or runner-recorded failure reason (e.g. runner_disconnected
		// — which marks a resumable task, not a dead one).
		suffix := ""
		if t.EndedAt > 0 && t.ExitCode != 0 {
			suffix += fmt.Sprintf("  exit=%d", t.ExitCode)
		}
		if len(t.ErrorMessage) > 0 {
			suffix += fmt.Sprintf("  err=%q", string(t.ErrorMessage))
		}
		resumedBy := ""
		if t.ResumedByKind != protocol.ClientKind_Unspecified {
			resumedBy = "  resumed_by=" + originStr(t.ResumedByKind)
		}
		createdBy := ""
		if t.CreatorTaskId.Id != ([16]byte{}) {
			createdBy = "  by=" + hex.EncodeToString(t.CreatorTaskId.Id[:])[:8]
		}
		caps := "  caps=" + CapsLabel(t.Capabilities)
		fmt.Fprintf(out, "  %s  %s  %s  repo=%s  from=%s%s%s%s%s  prompt=%q%s\n",
			taskIDStr(t.Id.Id[:]),
			taskStatusStr(t.Status),
			taskKindStr(t.Kind),
			string(t.RepoPath),
			originStr(t.OriginKind),
			agent,
			resumedBy,
			createdBy,
			caps,
			string(t.Prompt),
			suffix,
		)
	}
}

// agentStr renders a peer's agent descriptor for the ls output: the agent
// binary basename, plus "+skills" when the runner injects the harness skill.
// Empty bin renders as "?".
func agentStr(bin string, injected bool) string {
	if bin == "" {
		bin = "?"
	}
	if injected {
		return "agent=" + bin + "+skills"
	}
	return "agent=" + bin
}

// originStr formats a ClientKind for the `from=` column. Unspecified renders
// as "-" so a row visibly shows "no recorded origin" rather than the
// confusingly literal "unspecified" / "Unspecified" enum name.
func originStr(k protocol.ClientKind) string {
	if k == protocol.ClientKind_Unspecified {
		return "-"
	}
	return strings.ToLower(k.String())
}

// List (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).List instead.
func List(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.List(ctx, out)
}

func runnerStatusStr(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle   "
	case protocol.RunnerStatus_Busy:
		return "Busy   "
	default:
		return "Offline"
	}
}

// taskKindStr renders TaskKind as a fixed-width column. Vocabulary matches
// the TUI detail popup (tui/detail.go taskKindStr): oneshot / interactive.
// An interactive row explains its empty prompt= by itself; a oneshot row
// with an empty prompt really was submitted with one.
func taskKindStr(k protocol.TaskKind) string {
	switch k {
	case protocol.TaskKind_Oneshot:
		return "oneshot    "
	case protocol.TaskKind_Interactive:
		return "interactive"
	}
	return "?          "
}

func taskStatusStr(s protocol.TaskStatus) string {
	switch s {
	case protocol.TaskStatus_Queued:
		return "Queued   "
	case protocol.TaskStatus_Running:
		return "Running  "
	case protocol.TaskStatus_Succeeded:
		return "Succeeded"
	case protocol.TaskStatus_Failed:
		return "Failed   "
	case protocol.TaskStatus_Cancelled:
		return "Cancelled"
	case protocol.TaskStatus_Detached:
		return "Detached "
	}
	return "?"
}

// taskIDStr returns the full hex encoding of b, or "-" if every byte is zero.
// Full length is required so the printed value can be copy-pasted directly
// into harness-cli subcommands (cancel / logs / file push / file pull / ...).
func taskIDStr(b []byte) string {
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "-"
	}
	const tab = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, v := range b {
		out = append(out, tab[v>>4], tab[v&0xf])
	}
	return string(out)
}
