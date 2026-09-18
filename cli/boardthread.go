package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
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
	// Conversation is the key of the conversation this row's chain belongs to
	// — the participant set, or a named topic when the chain touched one. It is
	// stamped by SelectThreads and carried to every consumer, including the
	// browser, so nothing has to re-derive the grouping and disagree about it.
	//
	// Empty only when the row came from a path that does not group (nothing
	// does today; it is the zero value, not a state).
	Conversation string
	// Size is the PUBLISHED byte count. It is always populated: BuildThreads
	// fills it from the payload it was handed, and a face that collected
	// metadata without bodies (the agent face — ListRetained carries a size
	// but no body) overwrites it from that metadata.
	//
	// It is a field rather than len(Msg.Payload) at the point of use because
	// under --headers-only the agent face never fetches a body, and a renderer
	// reading len(Payload) there would print 0 and claim a zero-byte message
	// was published. It is always populated, rather than zero-means-unset,
	// because an empty publish is a real and documented case here (`agent
	// send` reporting bytes: 0) and a sentinel would make it unrepresentable.
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
			Size:   len(m.Payload),
		})
		if topicOf != nil {
			out[len(out)-1].Topic = topicOf[m.Seq]
		}
		kids := children[m.Seq]
		// Indent marks a FORK, not a reply. A message with exactly one reply is
		// a continuation and keeps its parent's depth; only where a message was
		// answered more than once does the view step right.
		//
		// The alternative — depth = reply count — is what this had first, and
		// it does not survive the shape agent conversations actually take.
		// Measured on the live board, 2026-09-18, on a supervisor/worker
		// exchange: 31 messages, maximum depth 17, and ZERO messages with more
		// than one reply. Fifty-one columns of gutter were spent encoding
		// nothing, because a strictly linear back-and-forth has nothing to
		// encode. cli/tasktree.go can indent per level because a spawn tree is
		// shallow by construction; a reply chain is as deep as the
		// conversation is long.
		//
		// No linkage is lost: every row carries re=<parent seq>, which names
		// the parent exactly, where the gutter could only ever imply it.
		for i, k := range kids {
			if len(kids) == 1 {
				walk(k, isLast)
				continue
			}
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
			row := ThreadRow{Msg: m, Depth: 0, Orphan: true, Size: len(m.Payload)}
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
		return groupConversations(rowsOfChain(rows, find, want), topicOf), nil
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
		return groupConversations(out, topicOf), nil
	}
	return groupConversations(rows, topicOf), nil
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

// CollectThreadsWith gathers every topic the caller can see, assembles the
// reply chains across all of them, and applies f.
//
// It exists so the CLI verb and the WebUI's wasm bridge run the SAME
// collection. A conversation spans topics by construction — each agent
// receives on its own chat.<short-id> — so "which topics" is part of the
// answer, not a caller's choice, and a second implementation on the browser
// side would be free to disagree about it.
//
// It takes a *Client rather than a ConnectionID because the surfaces that are
// not one-shot commands hold a long-lived one; CollectThreads is the
// dial-per-call wrapper for the ones that do not.
//
// A topic that vanishes between the listing and the read is skipped, not an
// error: topics die with their last subscriber (see the retract design's
// Amendment 2026-09-18d), so the race is ordinary rather than exceptional.
func CollectThreadsWith(ctx context.Context, c *Client, f ThreadFilter) ([]ThreadRow, error) {
	topics, err := c.BoardTopics(ctx)
	if err != nil {
		return nil, err
	}
	var msgs []BoardMessage
	topicOf := make(map[uint64]string)
	for _, t := range topics {
		tmsgs, found, err := c.BoardRead(ctx, t.Name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for _, m := range tmsgs {
			topicOf[m.Seq] = t.Name
		}
		msgs = append(msgs, tmsgs...)
	}
	return SelectThreads(BuildThreads(msgs, topicOf), topicOf, f)
}

// CollectThreads is CollectThreadsWith for a caller that has no client to
// reuse: it dials, collects and closes.
func CollectThreads(ctx context.Context, peerCID objproto.ConnectionID, f ThreadFilter) ([]ThreadRow, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return CollectThreadsWith(ctx, c, f)
}

// conversationKey identifies the conversation one chain belongs to.
//
// A chain that touched a topic which is NOT a chat.<short-id> keys on that
// topic: a sender who declared a subject with --reply-to said the subject is
// the unit, and this takes them at their word. The lowest-sorting such name
// wins so the key does not depend on message order.
//
// Otherwise the key is the participant set. A message on chat.<id> has TWO
// parties — whoever sent it and whoever owns that inbox — and the second is
// the half a sender-only rule would drop, which is the very asymmetry that
// makes a topic-keyed view show one side of an exchange.
func conversationKey(chain []ThreadRow, topicOf map[uint64]string) string {
	named := ""
	parties := map[string]bool{}
	for _, r := range chain {
		topic := r.Topic
		if topic == "" && topicOf != nil {
			topic = topicOf[r.Msg.Seq]
		}
		switch {
		case strings.HasPrefix(topic, "chat."):
			parties[strings.TrimPrefix(topic, "chat.")] = true
		case topic != "":
			if named == "" || topic < named {
				named = topic
			}
		}
		if h := r.Msg.FromTaskHex; h != "" {
			if len(h) > 8 {
				h = h[:8]
			}
			parties[h] = true
		}
	}
	if named != "" {
		return named
	}
	if len(parties) == 0 {
		// Reachable: a chain whose messages carry neither a topic nor an
		// attributable sender. Keyed apart from every real conversation rather
		// than folded into one of them.
		return "(unattributed)"
	}
	ids := make([]string, 0, len(parties))
	for p := range parties {
		ids = append(ids, p)
	}
	sort.Strings(ids)
	return strings.Join(ids, "+")
}

// groupConversations reorders rows into conversation sections and stamps each
// row with its key.
//
// A chain is not a conversation: an unanswered message is its own chain, and
// so is one sent with --topic rather than --in-reply-to. Measured on a live
// board, a two-party exchange five minutes long produced four chains and one
// conversation. Without this layer those fragments interleave with every other
// exchange's, ordered by seq, with nothing marking the boundary.
//
// Conversations are ordered by their most recent message, ASCENDING: this is a
// transcript and the view it extends already reads forward in time. Chains
// within one keep their relative order, and rows within a chain are untouched.
func groupConversations(rows []ThreadRow, topicOf map[uint64]string) []ThreadRow {
	if len(rows) == 0 {
		return rows
	}

	// Components over the rows that survived the filter — a chain whose other
	// half was filtered out is a chain of what is left, not a dangling link.
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
		if r.Msg.InReplyTo == 0 {
			continue
		}
		if _, ok := comp[r.Msg.InReplyTo]; !ok {
			continue
		}
		if a, b := find(r.Msg.Seq), find(r.Msg.InReplyTo); a != b {
			comp[a] = b
		}
	}

	type chain struct {
		rows   []ThreadRow
		key    string
		latest uint64 // ReceivedAtMs of its newest message
	}
	chains := map[uint64]*chain{}
	var chainOrder []uint64
	for _, r := range rows {
		id := find(r.Msg.Seq)
		c, ok := chains[id]
		if !ok {
			c = &chain{}
			chains[id] = c
			chainOrder = append(chainOrder, id)
		}
		c.rows = append(c.rows, r)
		if r.Msg.ReceivedAtMs > c.latest {
			c.latest = r.Msg.ReceivedAtMs
		}
	}

	type conv struct {
		key    string
		latest uint64
		chains []*chain
	}
	convs := map[string]*conv{}
	var convOrder []string
	for _, id := range chainOrder {
		c := chains[id]
		c.key = conversationKey(c.rows, topicOf)
		v, ok := convs[c.key]
		if !ok {
			v = &conv{key: c.key}
			convs[c.key] = v
			convOrder = append(convOrder, c.key)
		}
		v.chains = append(v.chains, c)
		if c.latest > v.latest {
			v.latest = c.latest
		}
	}

	// Ties broken by key so the order is total: two conversations can share a
	// millisecond, and an unstable order moves sections under a reader between
	// refreshes.
	sort.SliceStable(convOrder, func(i, j int) bool {
		a, b := convs[convOrder[i]], convs[convOrder[j]]
		if a.latest != b.latest {
			return a.latest < b.latest
		}
		return a.key < b.key
	})

	out := make([]ThreadRow, 0, len(rows))
	for _, k := range convOrder {
		for _, c := range convs[k].chains {
			for _, r := range c.rows {
				r.Conversation = k
				out = append(out, r)
			}
		}
	}
	return out
}

// ConversationHeader is the one spelling of a conversation's section header.
//
// Participants are named `<agent>/<task8>` where the row data says who they
// are. A party that only ever RECEIVED — it owns a chat.<id> topic and never
// sent — has no agent profile anywhere in the messages, so it appears as the
// bare id. That is missing information rather than a missing participant, and
// leaving it out of the header would hide half of a one-way exchange.
func ConversationHeader(rows []ThreadRow) string {
	if len(rows) == 0 {
		return ""
	}
	key := rows[0].Conversation
	agentOf := map[string]string{}
	parties := map[string]bool{}
	chains := map[uint64]bool{}
	var first, last uint64
	for _, r := range rows {
		if h := r.Msg.FromTaskHex; h != "" {
			if len(h) > 8 {
				h = h[:8]
			}
			parties[h] = true
			if a := r.Msg.FromAgentProfile; a != "" {
				agentOf[h] = a
			}
		}
		if strings.HasPrefix(r.Topic, "chat.") {
			parties[strings.TrimPrefix(r.Topic, "chat.")] = true
		}
		root := r.Msg.Seq
		if r.Msg.InReplyTo != 0 {
			root = r.Msg.InReplyTo
		}
		chains[root] = true
		if first == 0 || r.Msg.ReceivedAtMs < first {
			first = r.Msg.ReceivedAtMs
		}
		if r.Msg.ReceivedAtMs > last {
			last = r.Msg.ReceivedAtMs
		}
	}
	names := make([]string, 0, len(parties))
	for p := range parties {
		if a := agentOf[p]; a != "" {
			names = append(names, a+"/"+p)
		} else {
			names = append(names, p)
		}
	}
	sort.Strings(names)

	who := strings.Join(names, " ↔ ")
	if !strings.Contains(key, "+") && !strings.HasPrefix(key, "(") {
		// A named topic is the room; the participants are who showed up in it.
		who = key + "  (" + strings.Join(names, ", ") + ")"
	}
	span := boardMsToRFC3339(first)[11:19]
	if last != first {
		span += "–" + boardMsToRFC3339(last)[11:19]
	}
	return fmt.Sprintf("%s   %d message(s)   %s", who, len(rows), span)
}
