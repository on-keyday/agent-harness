package cli

import (
	"fmt"
	"sort"
	"strings"
)

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
	// Size is the PUBLISHED byte count, known to faces that collect metadata
	// without carrying payloads (the agent face under --headers-only; the
	// ListRetained metas carry Size but no body). Zero means "read
	// len(Msg.Payload) instead", which is the board face's always-true case.
	Size int
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

// ThreadFilter carries the two selector axes both faces of the thread view
// share. They compose as an AND when both are set.
type ThreadFilter struct {
	// Tasks: keep a chain if ANY message in it was sent by a named task OR
	// sits on that task's own inbound topic (chat.<id8>). Repeating the flag
	// UNIONS — two ids give the pair's exchange plus anything either had with
	// a third party, because a filter that dropped the third party would hide
	// the fact that the conversation was not private.
	Tasks []string
	// Seq: keep only the chain containing it. 0 = no selection.
	Seq uint64
}

// SeqNotVisibleError reports a --seq that names no message in the input set.
// The two faces render it differently — the operator reads "not in the
// visible set", the agent reads "not readable from this task" — so callers
// match on the type and word it for their reader; the default text is the
// operator's.
type SeqNotVisibleError struct {
	Seq uint64
}

func (e *SeqNotVisibleError) Error() string {
	return fmt.Sprintf("board thread: seq %d is not in the visible set (its topic may have died with its last subscriber task, or it rotated out of a topic's 64-message ring)", e.Seq)
}

// SelectThreads filters rows produced by BuildThreads to the chains the
// filter selects, returning them in BuildThreads order.
//
// Chains, not messages, are the unit: a reply links messages into one chain,
// and keep/drop applies to the whole of it. The chain id is the component
// root under a union over the in_reply_to links, so a malformed cycle still
// lands every member in one component instead of looping.
//
// A Seq that names no message in the input is a *SeqNotVisibleError, not an
// empty result — the same distinction board read draws when a topic holds
// messages but none reply to the requested seq. An empty result and a bad
// argument must not look the same. A Seq whose chain involves no named task
// is an empty result: the chain exists, the filter just does not select it.
func SelectThreads(rows []ThreadRow, topicOf map[uint64]string, f ThreadFilter) ([]ThreadRow, error) {
	comp := make(map[uint64]uint64, len(rows))
	var find func(uint64) uint64
	find = func(x uint64) uint64 {
		for comp[x] != x {
			comp[x] = comp[comp[x]]
			x = comp[x]
		}
		return x
	}
	for _, r := range rows {
		comp[r.Msg.Seq] = r.Msg.Seq
	}
	for _, r := range rows {
		if r.Msg.InReplyTo != 0 {
			if _, parentVisible := comp[r.Msg.InReplyTo]; parentVisible {
				rp, rt := find(r.Msg.Seq), find(r.Msg.InReplyTo)
				if rp != rt {
					comp[rp] = rt
				}
			}
		}
	}

	keepComp := map[uint64]bool{}
	haveTasks := false
	for _, t := range f.Tasks {
		id := strings.ToLower(strings.TrimSpace(t))
		if id == "" {
			continue
		}
		haveTasks = true
		prefix := id
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		chatTopic := "chat." + prefix
		for _, r := range rows {
			if r.Msg.FromTaskHex == id || (topicOf != nil && topicOf[r.Msg.Seq] == chatTopic) {
				keepComp[find(r.Msg.Seq)] = true
			}
		}
	}

	if f.Seq != 0 {
		want, found := comp[f.Seq]
		if !found {
			return nil, &SeqNotVisibleError{Seq: f.Seq}
		}
		if haveTasks && !keepComp[want] {
			// AND of the two axes: the chain exists but involves no named
			// task. An empty result is the honest answer — the bad argument
			// is the error above, not this.
			return nil, nil
		}
		return rowsOfChain(rows, find, want), nil
	}
	if haveTasks {
		// --task names at least one task. An empty keepComp here means the
		// named tasks match NOTHING in the visible set — an empty result,
		// never "everything": a filter that matched nothing must not fall
		// back to unfiltered, or `--task <mistyped>` would silently print
		// the whole board.
		if len(keepComp) == 0 {
			return nil, nil
		}
		out := make([]ThreadRow, 0, len(rows))
		for _, r := range rows {
			if keepComp[find(r.Msg.Seq)] {
				out = append(out, r)
			}
		}
		return out, nil
	}
	return rows, nil
}

// rowsOfChain keeps only the rows whose chain root is want. The rows arrive
// in pre-order, so a chain's members are contiguous except for unrelated
// roots interleaved by seq; filtering by component id preserves their order.
func rowsOfChain(rows []ThreadRow, find func(uint64) uint64, want uint64) []ThreadRow {
	out := make([]ThreadRow, 0, len(rows))
	for _, r := range rows {
		if find(r.Msg.Seq) == want {
			out = append(out, r)
		}
	}
	return out
}
