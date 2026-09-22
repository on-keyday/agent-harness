package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ThreadOp is which destructive verb a thread-scoped fan-out runs.
type ThreadOp int

const (
	// ThreadRetract withdraws each message from every agent-facing path,
	// leaving it readable to the operator marked RETRACTED.
	ThreadRetract ThreadOp = iota
	// ThreadPurge destroys the bytes, operator view included.
	ThreadPurge
)

// success is the outcome label a completed call gets, and the first category
// Summary prints.
func (o ThreadOp) success() string {
	if o == ThreadPurge {
		return "purged"
	}
	return "retracted"
}

// skipsWithdrawn reports whether a message the caller already knows is
// withdrawn should be left alone.
//
// The two verbs answer this OPPOSITELY, and the whole two-stage workflow rests
// on the difference. Retract has nothing to do to a withdrawn message. Purge
// does: withdrawLocked moves it out of topic.ring into topic.retracted and
// removeSeq scans both, so purge is exactly how the operator clears what
// retract withdrew. Skipping withdrawn rows on the purge path -- the rule that
// is right for retract -- would make stage 2 unable to reach stage 1's output.
func (o ThreadOp) skipsWithdrawn() bool { return o == ThreadRetract }

// ThreadRowResult is one message's outcome. Outcome takes exactly the values
// Summary counts, so the JSON form and the text form cannot disagree.
type ThreadRowResult struct {
	Seq     uint64 `json:"seq"`
	Topic   string `json:"topic"`
	Outcome string `json:"outcome"`
}

// ThreadFanoutResult is what one run did.
//
// Rows accounts for EVERY message in the thread, including the ones never
// called for, so a reader can reconcile it against what `board thread` showed.
// Err is the error that STOPPED the run; the rows after it carry "skipped".
type ThreadFanoutResult struct {
	Header     string
	Rows       []ThreadRowResult
	Err        error
	categories []string
}

// Counts totals the outcomes, with an explicit zero for every category this
// run's op can produce.
func (r ThreadFanoutResult) Counts() map[string]int {
	out := make(map[string]int, len(r.categories))
	for _, c := range r.categories {
		out[c] = 0
	}
	for _, row := range r.Rows {
		out[row.Outcome]++
	}
	return out
}

// Summary is the operator-facing line: every category with its count, zeros
// included.
//
// The category set comes from the op, not from the outcomes actually seen. A
// set derived from the rows would drop "retracted 0" on a run where every
// message was already withdrawn -- which is precisely the run where a reader
// most needs to see that nothing new was withdrawn.
func (r ThreadFanoutResult) Summary() string {
	counts := r.Counts()
	parts := make([]string, 0, len(r.categories))
	for _, c := range r.categories {
		parts = append(parts, fmt.Sprintf("%s %d", c, counts[c]))
	}
	return "  " + strings.Join(parts, "   ")
}

// fanoutRows walks rows in order, calling do for each row it should ask about,
// and stops at the first error.
//
// Split from FanoutThread so the partitioning, the stop rule and the counting
// are testable without a server. do reports found, in the sense both
// BoardRetract and BoardPurge use it: false is the server's single collapsed
// "no such topic / no such seq / already gone" answer.
func fanoutRows(rows []ThreadRow, header, success string, skipWithdrawn bool, do func(topic string, seq uint64) (bool, error)) ThreadFanoutResult {
	cats := []string{success}
	if skipWithdrawn {
		cats = append(cats, "already-withdrawn")
	}
	cats = append(cats, "not-found", "failed", "skipped")

	res := ThreadFanoutResult{Header: header, Rows: make([]ThreadRowResult, 0, len(rows)), categories: cats}
	for _, r := range rows {
		rr := ThreadRowResult{Seq: r.Msg.Seq, Topic: r.Topic}
		switch {
		case res.Err != nil:
			rr.Outcome = "skipped"
		case skipWithdrawn && r.Msg.Retracted:
			// Not asked about. The server would answer not-found -- it
			// collapses already-withdrawn into that answer so a reply cannot
			// probe a topic -- and reporting a handled message as not-found
			// renders success as failure.
			rr.Outcome = "already-withdrawn"
		default:
			found, err := do(r.Topic, r.Msg.Seq)
			switch {
			case err != nil:
				// A capability denial on one call is a denial on all of them,
				// so stop rather than issue the rest; the remainder is marked
				// skipped by the first arm on the next iteration.
				res.Err = err
				rr.Outcome = "failed"
			case !found:
				rr.Outcome = "not-found"
			default:
				rr.Outcome = success
			}
		}
		res.Rows = append(res.Rows, rr)
	}
	return res
}

// FanoutThread resolves f to a thread and runs op over every message in it, on
// the client it is given.
//
// ONE client for the whole run: a thread is tens of messages and the
// package-level BoardRetract / BoardPurge helpers dial per call.
//
// This is the only place the per-seq destructive calls are made on a thread
// path. The CLI verbs, the TUI action and the wasm bridge all reach it; a
// second loop elsewhere is how one surface silently grows different semantics
// from the others.
func FanoutThread(ctx context.Context, c *Client, op ThreadOp, f ThreadFilter) (ThreadFanoutResult, error) {
	rows, err := CollectThreadsWith(ctx, c, f)
	if err != nil {
		return ThreadFanoutResult{}, err
	}
	if len(rows) == 0 {
		// Reachable only through the axes ANDing to nothing: a selector that
		// names nothing at all is already an error from SelectThreads.
		return ThreadFanoutResult{}, fmt.Errorf("board: the selector matched no messages")
	}
	header := ConversationHeader(rows)

	// Act in seq order so a partial run is a prefix an operator can reason
	// about rather than an arbitrary subset. The rows arrive grouped for
	// reading, which is a different order.
	ordered := append([]ThreadRow(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Msg.Seq < ordered[j].Msg.Seq })

	return fanoutRows(ordered, header, op.success(), op.skipsWithdrawn(),
		func(topic string, seq uint64) (bool, error) {
			if op == ThreadPurge {
				_, found, perr := c.BoardPurge(ctx, topic, seq)
				return found, perr
			}
			return c.BoardRetract(ctx, topic, seq)
		}), nil
}
