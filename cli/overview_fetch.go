package cli

import (
	"context"
	"time"
)

// OverviewBoardWith fetches the board half of the overview over c: every
// topic's counts, and the newest message of the topics OverviewHeadTopics
// names. A head read that fails or finds the topic gone leaves that topic
// without a head; only the topic listing's failure is an error.
func (c *Client) OverviewBoardWith(ctx context.Context) ([]BoardTopicRow, map[string]*BoardMessage, error) {
	topics, err := c.BoardTopics(ctx)
	if err != nil {
		return nil, nil, err
	}
	newest := map[string]*BoardMessage{}
	for _, name := range OverviewHeadTopics(topics) {
		msgs, found, rerr := c.BoardReadPage(ctx, name, 0, 1)
		if rerr != nil || !found || len(msgs) == 0 {
			continue
		}
		m := msgs[0]
		newest[name] = &m
	}
	return topics, newest, nil
}

// OverviewWith fetches every input of the overview over c and builds it. Only
// the snapshot's failure is returned: conns, forwards and the board each
// carry their own error into the section they feed.
func (c *Client) OverviewWith(ctx context.Context, now time.Time) (Overview, error) {
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return Overview{}, err
	}
	in := OverviewInput{Runners: lr.Runners, Tasks: lr.Tasks}
	in.Conns, in.ConnsErr = c.ConnListWith(ctx)
	in.Forwards, in.ForwardsErr = c.PortForwardListWith(ctx, ForwardListQuery{})
	in.Topics, in.Newest, in.TopicsErr = c.OverviewBoardWith(ctx)
	return BuildOverview(in, now), nil
}
