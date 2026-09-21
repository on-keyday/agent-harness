package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Topics fetches the board-wide topic list and emits one JSON Lines record per topic.
func Topics(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("topics", args)
	if perr != nil {
		return perr
	}
	return TopicsWith(ctx, a, stdout)
}

// TopicsWith is Topics for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func TopicsWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID

	c, err := connectClient(ctx, *serverCID)
	if err != nil {
		return err
	}
	defer c.Close()

	// board_topics, not a verb of this face's own. The agent-side list_topics it
	// replaces read the same Board.ListTopics(), clamped msg_count the same way
	// and was gated on the same bit — it differed only in refusing with a status
	// value instead of PermissionDenied, and in NOT carrying retracted_count.
	// That second difference was drift, not policy: the same caller holding
	// board_observe could already get the field by typing `board topics`.
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardTopics}
	req.SetBoardTopics(protocol.BoardTopicsRequest{})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		// A denial arrives as *cli.CapabilityDeniedError, which names the
		// missing bit through CapsLabel. This verb used to hold the name
		// "board_observe" as a literal.
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_BoardTopics); err != nil {
		return err
	}
	r := resp.BoardTopics()
	if r == nil {
		return errors.New("topics: response variant is nil")
	}
	for _, s := range r.Topics {
		rec := map[string]any{
			"name":              string(s.Name),
			"last_seq":          s.LastSeq,
			"last_published_at": time.UnixMilli(int64(s.LastPublishedAtUnixMs)).UTC().Format(time.RFC3339),
			"msg_count":         s.MsgCount,
			// Withdrawn messages, counted separately and never folded into
			// msg_count: msg_count answers "how much would a subscriber
			// receive". Printed unconditionally, including at zero — gating a
			// field on its VALUE makes "none withdrawn" and "not reported"
			// the same row.
			"retracted_count": s.RetractedCount,
		}
		line, _ := json.Marshal(rec)
		fmt.Fprintln(stdout, string(line))
	}
	return nil
}
