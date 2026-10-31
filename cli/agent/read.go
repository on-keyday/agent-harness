package agent

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/on-keyday/agent-harness/cli/cliopts"
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
	if a.Topic != "" || a.Self {
		return readPage(ctx, a, stdout)
	}
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
	if r == nil || r.Status != protocol.AgentReadSeqStatus_Ok {
		// One sentence for four causes, because the server deliberately merges
		// them: distinguishing "not on a topic you subscribe to" from "gone"
		// would answer "does seq N exist?" for every seq on every ring.
		return fmt.Errorf("seq %d is not readable: it has rotated out of its topic's ring "+
			"(64 messages) or its 30-minute TTL, was purged, or is on a topic this task "+
			"does not subscribe to. A retained topic keeps older messages on disk: "+
			"agent read --topic <topic> --before %d", seq, seq+1)
	}
	msgs, rerr := readDelivered(ctx, c, r.StreamId)
	if rerr != nil {
		return fmt.Errorf("read seq=%d: %w", seq, rerr)
	}
	if len(msgs) != 1 {
		return fmt.Errorf("read seq=%d: server returned %d messages, want 1", seq, len(msgs))
	}
	emitMessageLine(stdout, msgs[0], msgs[0].Payload)
	return nil
}

// readPage is `agent read --topic|--self`: one page of a topic this task
// subscribes to, newest first, one line per message. On a retained topic the
// server reads past the ring into the disk log; agent read <seq> never does.
func readPage(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	topic := a.Topic
	if a.Self {
		tid, err := cliopts.ResolveTaskID("")
		if err != nil {
			return err
		}
		topic = SelfTopic(tid)
	}

	c, cerr := connectClient(ctx, a.ServerCID)
	if cerr != nil {
		return cerr
	}
	defer c.Close()

	pr := protocol.AgentReadPageRequest{BeforeSeq: a.Before, Limit: uint16(a.Limit)}
	pr.SetTopic([]byte(topic))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentReadPage}
	req.SetAgentReadPage(pr)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentReadPage); err != nil {
		return err
	}
	r := resp.AgentReadPage()
	switch {
	case r == nil:
		return fmt.Errorf("read --topic %s: response variant is nil", topic)
	case r.Status == protocol.AgentReadPageStatus_NotFound:
		// Merged by the server, as for <seq>: "no such topic" and "not one you
		// subscribe to" must not be told apart.
		return fmt.Errorf("topic %q is not readable: no such topic, or not one this task subscribes to", topic)
	case r.Status != protocol.AgentReadPageStatus_Ok:
		return fmt.Errorf("read --topic %s: the server could not read the topic's history (%v)", topic, r.Status)
	}
	msgs, rerr := readDelivered(ctx, c, r.StreamId)
	if rerr != nil {
		return fmt.Errorf("read --topic %s: %w", topic, rerr)
	}
	for _, m := range msgs {
		emitMessageLine(stdout, m, m.Payload)
	}
	return nil
}
