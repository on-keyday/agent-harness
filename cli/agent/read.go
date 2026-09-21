package agent

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Read is the entry for `harness-cli agent read <seq>`: one retained message,
// addressed directly. It is what the inbox hooks point at when a body is too
// large to inline, so it never truncates — the whole reason to run it is to
// get the body the hook withheld.
//
// Reading is scoped to topics this task subscribes to. A seq outside them is
// reported exactly like one that has rotated out of its ring, so the error
// names both possibilities rather than confirming which.
func Read(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("read", args)
	if perr != nil {
		return perr
	}
	return ReadWith(ctx, a, stdout)
}

// ReadWith is Read for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func ReadWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID
	seq, perr := strconv.ParseUint(fmt.Sprint(a.Seq), 10, 64)
	if perr != nil || seq == 0 {
		return fmt.Errorf("seq must be a positive integer, got %q", fmt.Sprint(a.Seq))
	}

	c, cerr := connectClient(ctx, *serverCID)
	if cerr != nil {
		return cerr
	}
	defer c.Close()

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentReadSeq}
	req.SetAgentReadSeq(protocol.AgentReadSeqRequest{Seq: seq})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentReadSeq); err != nil {
		return err
	}
	r := resp.AgentReadSeq()
	if r == nil || r.Status != protocol.AgentReadSeqStatus_Ok || len(r.Msgs) == 0 {
		// One sentence for four causes, because the server deliberately merges
		// them: distinguishing "not on a topic you subscribe to" from "gone"
		// would answer "does seq N exist?" for every seq on every ring.
		return fmt.Errorf("seq %d is not readable: it has rotated out of its topic's ring "+
			"(64 messages) or its 30-minute TTL, was purged, or is on a topic this task "+
			"does not subscribe to", seq)
	}
	m := r.Msgs[0]
	payload, perr := fetchDeliveredPayload(ctx, c.Transport(), m.PayloadStreamId)
	if perr != nil {
		return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
	}
	emitMessageLine(stdout, m, payload)
	return nil
}
