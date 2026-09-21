package agent

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Retract is the entry for `harness-cli agent retract <seq>`: withdraw ONE
// message this task published. The message leaves every agent-facing path
// (deliver / inbox / wait / read_seq / retained) and survives only on the
// operator surfaces (`harness-cli board read`, TUI, WebUI), where it shows as
// retracted until it ages out with the topic.
//
// What it is for: a recipient whose context was reset re-reads a topic's
// retained ring and re-executes instructions it already carried out. Only the
// SENDER knows an instruction is spent, so withdrawing it has to be the
// sender's move — the reset recipient has nothing to distinguish handled from
// unhandled, which is the failure itself.
//
// No capability is required. The check is authorship: the server withdraws the
// message only when its recorded sender is this task, so retract can reach
// nothing the caller did not write. `agent purge` still needs Capability_Purge
// — it erases the bytes for real, including from the operator's view, and can
// take a whole topic of other agents' unread messages with it.
//
// Seq comes from the `seq` field this task got back when it sent the message
// (`agent send` prints it), or from `agent retained --topic <t>`, which lists
// each retained message's seq and sender.
func Retract(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("retract", args)
	if perr != nil {
		return perr
	}
	return RetractWith(ctx, a, stdout)
}

// RetractWith is Retract for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func RetractWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID
	seq, perr := strconv.ParseUint(fmt.Sprint(a.Seq), 10, 64)
	if perr != nil || seq == 0 {
		return fmt.Errorf("seq must be a positive integer, got %q", fmt.Sprint(a.Seq))
	}

	c, err := connectClient(ctx, *serverCID)
	if err != nil {
		return err
	}
	defer c.Close()

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentRetract}
	req.SetAgentRetract(protocol.AgentRetractRequest{Seq: seq})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentRetract); err != nil {
		return err
	}
	r := resp.AgentRetract()
	if r == nil {
		return fmt.Errorf("retract: response variant is nil")
	}
	switch r.Status {
	case protocol.AgentRetractStatus_Ok:
		fmt.Fprintf(stdout, "{\"status\":\"ok\",\"seq\":%d}\n", seq)
		return nil
	case protocol.AgentRetractStatus_NotFound:
		// Idempotent, and deliberately blind: "no live message with that seq"
		// (never published, rotated out, already retracted, purged) and
		// "published by somebody else" are one answer. seq is board-global and
		// consecutive, so separating them would confirm the existence of any
		// seq on any topic the caller cannot name.
		fmt.Fprintf(stdout, "{\"status\":\"not_found\",\"seq\":%d}\n", seq)
		return nil
	default:
		return fmt.Errorf("retract: unexpected status %v", r.Status)
	}
}
