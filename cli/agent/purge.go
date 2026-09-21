package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Purge is the entry for `harness-cli agent purge`. It destroys a topic's
// retained-message ring on the server (Capability_Purge required), flushing an
// unwanted payload out of the server-side buffer so a since=0 re-read can't
// resurface it. The cursor stays valid: the board seq counter is global, so a
// post-purge message gets a strictly higher seq.
func Purge(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("purge", args)
	if perr != nil {
		return perr
	}
	return PurgeWith(ctx, a, stdout)
}

// PurgeWith is Purge for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func PurgeWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID
	topic := &a.Topic
	self := &a.Self
	seq := &a.Seq
	if *self && *topic != "" {
		return errors.New("--self and --topic are mutually exclusive")
	}
	if *self {
		tid, err := cliopts.ResolveTaskID("")
		if err != nil {
			return err
		}
		t := SelfTopic(tid)
		topic = &t
	}
	if *topic == "" {
		return errors.New("--topic or --self required")
	}

	c, err := connectClient(ctx, *serverCID)
	if err != nil {
		return err
	}
	defer c.Close()

	// board_purge, not a verb of this face's own: the two handlers branched
	// identically on the same Board.PurgeTopic / PurgeSeq calls, clamped the
	// same way, and required the same bit. What differed was the status enum's
	// name and where the capability check was written.
	pr := protocol.BoardPurgeRequest{Seq: *seq}
	pr.SetTopic([]byte(*topic))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardPurge}
	req.SetBoardPurge(pr)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		// A denial arrives as *cli.CapabilityDeniedError, naming the bit via
		// CapsLabel. This verb used to hold the name "purge" as a literal.
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_BoardPurge); err != nil {
		return err
	}
	r := resp.BoardPurge()
	if r == nil {
		return errors.New("purge: response variant is nil")
	}
	switch r.Status {
	case protocol.BoardStatus_Ok:
		fmt.Fprintf(stdout, "{\"status\":\"ok\",\"topic\":%q,\"purged\":%d}\n", *topic, r.Purged)
		return nil
	case protocol.BoardStatus_NotFound:
		// Idempotent: nothing matched (topic never created / already evicted, or
		// --seq named a message no longer in the ring). Not an error.
		fmt.Fprintf(stdout, "{\"status\":\"not_found\",\"topic\":%q,\"purged\":0}\n", *topic)
		return nil
	default:
		return fmt.Errorf("purge: unexpected status %v", r.Status)
	}
}
