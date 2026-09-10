package cli

import (
	"fmt"
	"sort"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TrsfRow is one connection's transport reading with every cell already
// rendered.
//
// Strings rather than numbers, because half these cells can be ABSENT and the
// difference from zero is the point: a counter the answerer does not report, a
// delta with no previous reading behind it, and a share of an interval nothing
// parked in are all "-", while a counter that did not move is a measured "0".
// Handing each surface raw numbers would make all three re-decide that, and
// they would disagree — the shape `scopeSpecFor` (Go) and `scopeSpecJS` (JS)
// took before both were deleted in favour of one exported serializer.
//
// So this is the serializer for the trsf reading: the CLI table, the TUI modal
// and the WebUI panel all render these strings and none of them does
// arithmetic on a counter.
type TrsfRow struct {
	CID string
	// The wire spelling, e.g. "Runner". A surface whose neighbouring columns
	// use a lowercase convention applies it — the TUI's conns modal already
	// does that for its identity rows, and matching it there beats making the
	// CLI's long-standing output a casualty of the move.
	Role     string
	Task     string // principal task, 8-hex head; "-" for a runner or non-agent conn
	Cwnd     string
	InFlight string
	SRTT     string
	Queue    string // srtt - min_rtt: how much of the round trip is a queue rather than the path
	LossD    string
	SpurD    string
	LoopD    string
	BlockPct string
	Wait     string
}

// TrsfSampler turns a series of readings into rows, holding the previous one so
// the delta columns have something to be a delta OF.
//
// The zero value is ready: the first Observe has no previous reading and every
// rate column comes back "-".
//
// It exists because several of these counters mean nothing as a single sample —
// loop_iterations separates a run loop that is BLOCKED from one that is
// BUSY-SPINNING only across two reads, and BLOCK%/WAIT exist only as a delta —
// so every surface that shows them has to keep this state. Keeping it here
// keeps the interval right in one place: BLOCK% is a share of the interval the
// counters advanced over, which happened on the ANSWERING host, so it is
// divided by the difference of two of its own timestamps. Measured locally
// instead, it divides one host's delta by another's elapsed, which is how a
// share-of-the-interval column came to print 135%.
//
// The previous reading is REPLACED, not merged, so a connection that goes away
// and comes back is a first sighting again. The CLI's own loop merged, which
// meant such a row compared against a reading two or more intervals old while
// elapsed still measured one — a rate over the wrong denominator, silently.
type TrsfSampler struct {
	prev   map[string]protocol.TrsfConnState
	prevAt int64
}

// advance swaps in this reading and hands back the one it replaced, plus how
// long the answerer says passed between the two. Both zero on the first call.
func (s *TrsfSampler) advance(conns []protocol.TrsfConnState, sampledAt int64) (map[string]protocol.TrsfConnState, time.Duration) {
	prev := s.prev
	var elapsed time.Duration
	if s.prevAt != 0 && sampledAt > s.prevAt {
		elapsed = time.Duration(sampledAt - s.prevAt)
	}
	next := make(map[string]protocol.TrsfConnState, len(conns))
	for i := range conns {
		next[string(conns[i].Cid)] = conns[i]
	}
	s.prev, s.prevAt = next, sampledAt
	return prev, elapsed
}

// Observe records this reading and renders it. sampledAt is when the ANSWERER
// walked its connections, by the answerer's own clock.
//
// Observe and ObserveJSON are alternatives, not a pair: each advances the
// sampler, so calling both on one reading would consume it twice and leave the
// second comparing a reading against itself.
func (s *TrsfSampler) Observe(conns []protocol.TrsfConnState, sampledAt int64) []TrsfRow {
	prev, elapsed := s.advance(conns, sampledAt)
	// By cid, because the answerer has none: it ranges a Go map, so the same
	// connections come back in a different order every reading. On the CLI
	// that is only annoying; in a table with a cursor it is wrong — the row
	// under the selection changes once a second, so a key aimed at the runner
	// row lands on whatever took its place. Found by pressing enter on one.
	// renderConnTopology sorts its IP clusters for the same reason.
	conns = append([]protocol.TrsfConnState(nil), conns...)
	sort.Slice(conns, func(i, j int) bool {
		return string(conns[i].Cid) < string(conns[j].Cid)
	})
	rows := make([]TrsfRow, 0, len(conns))
	for i := range conns {
		r := &conns[i]
		row := TrsfRow{
			CID:      string(r.Cid),
			Role:     r.Role.String(),
			Task:     PrincipalShort(r.PrincipalTask.Id[:]),
			Cwnd:     fmt.Sprintf("%d", r.CounterOr(protocol.TrsfCounterKey_Cwnd, 0)),
			InFlight: fmt.Sprintf("%d", r.CounterOr(protocol.TrsfCounterKey_BytesInFlight, 0)),
			SRTT:     (time.Duration(r.CounterOr(protocol.TrsfCounterKey_SrttUs, 0)) * time.Microsecond).String(),
			Queue:    trsfQueueDelay(r),
			LossD:    "-", SpurD: "-", LoopD: "-", BlockPct: "-", Wait: "-",
		}
		if p, ok := prev[string(r.Cid)]; ok {
			row.LossD = trsfDeltaStr(r, &p, protocol.TrsfCounterKey_LossEvents)
			row.SpurD = trsfDeltaStr(r, &p, protocol.TrsfCounterKey_LossSpurious)
			row.LoopD = trsfDeltaStr(r, &p, protocol.TrsfCounterKey_LoopIterations)
			row.BlockPct, row.Wait = trsfParkSummary(r, &p, elapsed)
		}
		rows = append(rows, row)
	}
	return rows
}

// ObserveJSON is Observe's machine-readable form: every counter the answerer
// sent, by its own name, plus a delta for each when there is something to
// compare against.
//
// Nothing here enumerates the counters. That is the point of the keyed row: a
// counter added to the transport reaches this output with no edit, so no client
// is one of the places a new measurement has to be threaded through — and a key
// this build does not know still appears, under the enum's numeric fallback
// name, rather than being silently dropped.
func (s *TrsfSampler) ObserveJSON(conns []protocol.TrsfConnState, sampledAt int64) []map[string]any {
	prev, _ := s.advance(conns, sampledAt)
	// Same cid order as Observe: a --watch consumer diffing successive
	// readings should not see the lines permute under it either.
	conns = append([]protocol.TrsfConnState(nil), conns...)
	sort.Slice(conns, func(i, j int) bool {
		return string(conns[i].Cid) < string(conns[j].Cid)
	})
	out := make([]map[string]any, 0, len(conns))
	for i := range conns {
		r := &conns[i]
		m := map[string]any{"cid": string(r.Cid), "role": r.Role.String()}
		for _, c := range r.Counters {
			m[c.Key.String()] = c.Value
		}
		if r.PrincipalTask.Id != ([16]uint8{}) {
			m["principal_task"] = taskIDStr(r.PrincipalTask.Id[:])
		}
		// Deltas only when there IS a previous reading, so a consumer can tell
		// "no change" from "nothing to compare against".
		if p, ok := prev[string(r.Cid)]; ok {
			for _, c := range r.Counters {
				if d, ok := trsfDelta(r, &p, c.Key); ok {
					m[c.Key.String()+"_delta"] = d
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// trsfDelta is the change in one counter between two readings, and whether that
// change is DEFINED. An answerer older than a key omits it, so a missing key on
// either side means "this end does not report it" — a different answer from
// zero, which must not render as one.
func trsfDelta(r, p *protocol.TrsfConnState, k protocol.TrsfCounterKey) (uint64, bool) {
	rv, rok := r.Counter(k)
	pv, pok := p.Counter(k)
	if !rok || !pok {
		return 0, false
	}
	return rv - pv, true
}

func trsfDeltaStr(r, p *protocol.TrsfConnState, k protocol.TrsfCounterKey) string {
	if d, ok := trsfDelta(r, p, k); ok {
		return fmt.Sprintf("%d", d)
	}
	return "-"
}

// trsfParkSummary renders the derived columns the wake counters exist for: how
// much of the interval the run loop spent parked, and what ended those parks.
// Both are undefined without a previous reading, and BLOCK% is also undefined
// without elapsed time, so each returns "-" for ABSENCE — never for a zero,
// which is a measurement and prints as 0%.
//
// The raw counters are not columns. Every one is in the JSON form with its
// delta, because a table carrying them all stops being readable at the width
// where this one is already uncomfortable.
//
// elapsed is measured between the two readings by the ANSWERER's clock, so
// BLOCK% is a share of the interval the counters actually advanced over.
func trsfParkSummary(r, p *protocol.TrsfConnState, elapsed time.Duration) (blockPct, wait string) {
	blockPct, wait = "-", "-"
	if blocked, ok := trsfDelta(r, p, protocol.TrsfCounterKey_BlockedNs); ok && elapsed > 0 {
		blockPct = fmt.Sprintf("%.0f%%", 100*float64(blocked)/float64(elapsed))
	}
	parks, ok := trsfDelta(r, p, protocol.TrsfCounterKey_Blocks)
	if !ok || parks == 0 {
		// No park in the interval: the loop either never stopped or never ran.
		// A share of nothing has no subject, so it is absent rather than zero.
		return blockPct, wait
	}
	timer, _ := trsfDelta(r, p, protocol.TrsfCounterKey_WakeTimer)
	send, _ := trsfDelta(r, p, protocol.TrsfCounterKey_WakeSend)
	peer := parks - timer - send
	switch {
	case timer >= send && timer >= peer:
		// armed_pacer counts how a park was ARMED, not what ended it, so this
		// is a majority reading rather than an identity: it says most parks in
		// this interval carried the pacer's deadline, and the pacer's own floor
		// is 1 ms.
		pacer, _ := trsfDelta(r, p, protocol.TrsfCounterKey_ArmedPacer)
		if pacer > parks/2 {
			wait = "timer/pacer"
		} else {
			wait = "timer/loss"
		}
	case send >= peer:
		// "send" names the CHANNEL, and the channel is many-to-one: the same
		// wake follows the application supplying data and an ACK retiring a
		// range, which support opposite conclusions. So the label carries the
		// dominant PUSH reason, counted where each push is made.
		wait = "send/" + trsfDominantPush(r, p)
	default:
		wait = "peer" // an inbound packet, an ACK to send, a window update
	}
	return blockPct, wait
}

// trsfDominantPush names which kind of event pushed the send trigger most over
// the interval. Not a partition of the wakes — these are event counts, and
// several collapse onto one wake — so it answers "what mostly wanted the loop to
// run", which is the question "send" on its own cannot.
func trsfDominantPush(r, p *protocol.TrsfConnState) string {
	best, name := uint64(0), "?"
	for _, c := range []struct {
		n string
		k protocol.TrsfCounterKey
	}{
		{"app", protocol.TrsfCounterKey_SendPushApp},   // waiting on its caller
		{"ack", protocol.TrsfCounterKey_SendPushAck},   // the window was the constraint
		{"self", protocol.TrsfCounterKey_SendPushSelf}, // cycling, not waiting
		{"cwnd", protocol.TrsfCounterKey_SendPushCwnd}, // congestion-blocked, revived
		{"loss", protocol.TrsfCounterKey_SendPushLoss}, // retransmission pressure
		{"other", protocol.TrsfCounterKey_SendPushOther},
	} {
		if d, ok := trsfDelta(r, p, c.k); ok && d > best {
			best, name = d, c.n
		}
	}
	return name
}

// trsfQueueDelay is srtt - min_rtt: how much of the round trip is a queue rather
// than the path. The window's own drain time can BE the srtt, in which case
// cwnd/srtt equals the delivered rate for any cwnd and says nothing; this is
// the reading that separates the two.
func trsfQueueDelay(r *protocol.TrsfConnState) string {
	minRTT, ok := r.Counter(protocol.TrsfCounterKey_MinRttUs)
	if !ok {
		return "-" // no ACK has arrived: not measured, which is not zero
	}
	srtt := r.CounterOr(protocol.TrsfCounterKey_SrttUs, 0)
	if srtt < minRTT {
		return "0s"
	}
	return (time.Duration(srtt-minRTT) * time.Microsecond).String()
}
