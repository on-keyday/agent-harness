package cli

import "sort"

// ThreadRow is one board message placed in its reply chain, flattened into the
// order a renderer draws it in.
//
// Flattened rather than nested for the reason TaskTreeRow's comment states:
// the CLI, the TUI and the WebUI all draw "rows in order", so handing each of
// them a nested structure would make all three walk the reply links
// themselves — three walks that can and will disagree. One walk here, three
// dumb renderers.
type ThreadRow struct {
	Msg BoardMessage
	// Topic is which topic retained the message. A chain spans topics by
	// construction — each agent receives on its own chat.<short-id>, so one
	// exchange lives under several topics — and the renderer shows where each
	// message landed. A seq absent from the topicOf map yields "".
	Topic string
	Depth int
	// IsLast[d] reports whether the row is the last child at depth d. It is
	// what decides └─ versus ├─ at the row's own level, and whether each
	// ancestor column still needs a │ drawn through it — the same contract
	// TaskTreeRow.IsLast has, so TreePrefix renders both.
	IsLast []bool
	// Orphan marks a message whose in_reply_to names a seq not in the input
	// set — the parent rotated out of its ring, TTL-expired, was purged, or
	// sits on a topic this caller cannot read. All four are normal (the board
	// keeps 64 messages per topic for 30 minutes), so the message is shown at
	// root rather than hidden: a tree view re-orders a listing, it never
	// filters one.
	Orphan bool
}

// BuildThreads arranges messages under the messages they reply to, across
// every topic at once.
//
// Roots are the messages with InReplyTo == 0 plus every orphan. Siblings are
// ordered by Seq ascending: board seq is globally monotonic, so this is a
// total order and stable across polls. ReceivedAtMs is not — two messages can
// share a millisecond, and an unstable order moves rows under a reader's
// cursor between refreshes.
//
// Every input message comes back exactly once. The visited set that
// guarantees it also makes a cycle in the in_reply_to links terminate; the
// server mints seq monotonically and a reply always names an earlier seq, so
// a cycle is unreachable in normal operation — this is defence against a
// malformed or hand-built input set, matching BuildTaskTree's treatment of a
// creator cycle. Cycle members the walk cannot reach go at the end as depth-0
// rows rather than vanishing.
func BuildThreads(msgs []BoardMessage, topicOf map[uint64]string) []ThreadRow {
	if len(msgs) == 0 {
		return nil
	}

	present := make(map[uint64]bool, len(msgs))
	for _, m := range msgs {
		present[m.Seq] = true
	}

	children := make(map[uint64][]BoardMessage, len(msgs))
	var roots []BoardMessage
	orphan := make(map[uint64]bool)
	for _, m := range msgs {
		switch {
		case m.InReplyTo == 0:
			roots = append(roots, m)
		case !present[m.InReplyTo]:
			// Parent gone from the visible set: surface at root, flagged.
			orphan[m.Seq] = true
			roots = append(roots, m)
		default:
			children[m.InReplyTo] = append(children[m.InReplyTo], m)
		}
	}

	bySeq := func(s []BoardMessage) {
		sort.SliceStable(s, func(i, j int) bool { return s[i].Seq < s[j].Seq })
	}
	bySeq(roots)
	for k := range children {
		bySeq(children[k])
	}

	out := make([]ThreadRow, 0, len(msgs))
	visited := make(map[uint64]bool, len(msgs))

	var walk func(m BoardMessage, isLast []bool)
	walk = func(m BoardMessage, isLast []bool) {
		if visited[m.Seq] {
			return
		}
		visited[m.Seq] = true
		out = append(out, ThreadRow{
			Msg: m,
			// Depth is len(isLast) rather than a separate counter so the two
			// can never disagree about where the row sits.
			Depth:  len(isLast),
			IsLast: append([]bool(nil), isLast...),
			Orphan: orphan[m.Seq],
		})
		if topicOf != nil {
			out[len(out)-1].Topic = topicOf[m.Seq]
		}
		kids := children[m.Seq]
		for i, k := range kids {
			walk(k, append(isLast, i == len(kids)-1))
		}
	}
	for _, r := range roots {
		walk(r, nil)
	}

	// A cycle leaves its members unreachable from any root. They are still
	// messages the caller can see, so they go at the end rather than vanishing.
	for _, m := range msgs {
		if !visited[m.Seq] {
			visited[m.Seq] = true
			row := ThreadRow{Msg: m, Depth: 0, Orphan: true}
			if topicOf != nil {
				row.Topic = topicOf[m.Seq]
			}
			out = append(out, row)
		}
	}
	return out
}
