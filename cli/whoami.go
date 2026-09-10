package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// WhoAmIWith sends a whoami TaskControl request over an already-connected
// client and returns the server's decoded self-identity response. No
// capability is required: the answer is the caller's OWN principal plus the
// capability set the server enforces for THIS connection (callerCaps),
// resolved from the connection principal rather than anything the client
// sends. Long-lived embedders (TUI/WebUI) that already hold a *Client call
// this form; short-lived processes use WhoAmI.
func WhoAmIWith(ctx context.Context, c taskControlClient) (protocol.WhoAmIResponse, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Whoami}
	req.SetWhoami(protocol.WhoAmIRequest{Reserved: 0})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return protocol.WhoAmIResponse{}, err
	}
	if resp.Kind != protocol.TaskControlKind_Whoami {
		return protocol.WhoAmIResponse{}, fmt.Errorf("unexpected response kind: %v (want Whoami)", resp.Kind)
	}
	w := resp.Whoami()
	if w == nil {
		return protocol.WhoAmIResponse{}, fmt.Errorf("response missing Whoami variant")
	}
	return *w, nil
}

// WhoAmI dials the server and returns the decoded whoami response. Identity
// (agent vs operator) is auto-selected from the env by the shared Dial path —
// exactly as every other harness-cli subcommand, so an in-task agent reports
// its confined caps and an operator shell reports operator/all.
func WhoAmI(ctx context.Context, serverCID objproto.ConnectionID) (protocol.WhoAmIResponse, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return protocol.WhoAmIResponse{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return WhoAmIWith(ctx, c)
}

// isZeroTaskID reports whether a TaskID is all-zero (the operator / no-creator
// sentinel used throughout the protocol).
func isZeroTaskID(t protocol.TaskID) bool { return t.Id == ([16]byte{}) }

// ServerRevisionLabel renders the server's build for a human: the full commit,
// or an explicit unknown.
//
// Never elided, and that is the point rather than a style choice. An empty
// revision is a REAL answer — a server built with -buildvcs=false carries none
// — and a line that simply omits the field cannot be told from a server too old
// to send one. The two demand opposite actions (rebuild it properly vs restart
// it), so they must not look the same.
func ServerRevisionLabel(resp protocol.WhoAmIResponse) string {
	rev := string(resp.ServerRevision)
	if rev == "" {
		return "unknown (server built without VCS stamping)"
	}
	if resp.ServerDirty() {
		return rev + " DIRTY"
	}
	return rev
}

// WriteWhoAmI renders a WhoAmIResponse to out. Human form is two lines:
//
//	operator                                  caps=all
//	server=<full-hex>
//	task=<full-hex>  by=<creator8>            caps=spawn,file_read
//	server=<full-hex> DIRTY
//
// An all-zero principal means an operator connection (no confined principal →
// full authority). The server line is its own line rather than a suffix because
// it is about a different subject — the process answering, not the caller — and
// it is the one field here somebody greps for during a deploy.
//
// JSON form emits the same fields with hex task ids ("" when zero) for
// scripting.
func WriteWhoAmI(out io.Writer, resp protocol.WhoAmIResponse, asJSON bool) error {
	operator := isZeroTaskID(resp.PrincipalTaskId)
	if asJSON {
		taskHex := ""
		if !operator {
			taskHex = hex.EncodeToString(resp.PrincipalTaskId.Id[:])
		}
		creatorHex := ""
		if !isZeroTaskID(resp.CreatorTaskId) {
			creatorHex = hex.EncodeToString(resp.CreatorTaskId.Id[:])
		}
		// Hand-built rather than encoding/json because the field ORDER is part
		// of what a reader greps; scope_by_cap is marshalled on its own so the
		// map still escapes correctly.
		byCap, err := json.Marshal(ResolvedScopeByCap(resp.Capabilities, resp.Scope, resp.Overrides))
		if err != nil {
			return err
		}
		// server_revision is the RAW value, empty string included: item 22's
		// rule (a machine surface reports the value, a human surface labels
		// it), so a script can compare it against a sha without parsing the
		// "unknown …" prose the text form prints.
		_, err = fmt.Fprintf(out,
			"{\"operator\":%t,\"principal_task_id\":%q,\"creator_task_id\":%q,\"capabilities\":%q,\"scope\":%q,\"scope_by_cap\":%s,\"server_revision\":%q,\"server_dirty\":%t}\n",
			operator, taskHex, creatorHex, CapsLabel(resp.Capabilities), ScopeLabel(resp.Scope), byCap,
			string(resp.ServerRevision), resp.ServerDirty())
		return err
	}
	caps := "caps=" + CapsLabel(resp.Capabilities)
	// Always printed, subtree included. whoami answers "what am I allowed to
	// do", and scope is half that answer — omitting it when it happens to be the
	// default leaves a caller unable to tell a subtree scope from a whoami that
	// does not report scope at all. The --json form always carried it.
	caps += "  scope=" + ScopeLabel(resp.Scope)
	if ov := OverridesLabel(resp.Overrides); ov != "" {
		caps += " +" + ov
	}
	if operator {
		_, err := fmt.Fprintf(out, "operator  %s\nserver=%s\n", caps, ServerRevisionLabel(resp))
		return err
	}
	by := ""
	if !isZeroTaskID(resp.CreatorTaskId) {
		by = "  by=" + hex.EncodeToString(resp.CreatorTaskId.Id[:])[:8]
	}
	_, err := fmt.Fprintf(out, "task=%s%s  %s\nserver=%s\n",
		hex.EncodeToString(resp.PrincipalTaskId.Id[:]), by, caps, ServerRevisionLabel(resp))
	return err
}
