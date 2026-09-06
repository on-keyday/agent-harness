package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runTrsf prints congestion state, once or repeatedly.
//
// The repeating form is not a convenience. Several of these counters mean
// nothing as a single sample: loop_iterations separates a run loop that is
// BLOCKED (frozen across two reads) from one that is BUSY-SPINNING (exploding)
// from one that is merely congestion-blocked (advancing slowly), and the loss
// counters only say anything as a rate. So --watch prints the delta, and the
// one-shot form prints the absolute values it has.
func runTrsf(ctx context.Context, c *cli.Client, runnerCID, watch string, asJSON bool, out io.Writer) error {
	if watch == "" {
		rows, _, err := c.TrsfStateOn(ctx, runnerCID)
		if err != nil {
			return err
		}
		return writeTrsf(out, rows, nil, 0, 0, asJSON)
	}
	every, err := time.ParseDuration(watch)
	if err != nil {
		return fmt.Errorf("--watch %q: %w", watch, err)
	}
	if every <= 0 {
		return fmt.Errorf("--watch %q: must be positive", watch)
	}
	prev := map[string]protocol.TrsfConnState{}
	// When the ANSWERER sampled the previous reading, by its own clock — not
	// this process's, and not the ticker interval. BLOCK% is a share of the
	// interval the counters advanced over, which happened on the answering
	// host; measuring it here divides that delta by a local elapsed that also
	// contains a round trip whose length changes between readings. Doing so
	// printed 135%, which a share of an interval cannot be.
	var prevAt int64
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rows, sampledAt, err := c.TrsfStateOn(ctx, runnerCID)
		if err != nil {
			return err
		}
		if err := writeTrsf(out, rows, prev, prevAt, sampledAt, asJSON); err != nil {
			return err
		}
		for _, r := range rows {
			prev[string(r.Cid)] = r
		}
		prevAt = sampledAt
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// delta is the change in one counter between two readings, and whether that
// change is DEFINED. An answerer older than a key omits it, so a missing key on
// either side means "this end does not report it" — which is a different answer
// from zero and must not render as one.
func delta(r, p protocol.TrsfConnState, k protocol.TrsfCounterKey) (uint64, bool) {
	rv, rok := r.Counter(k)
	pv, pok := p.Counter(k)
	if !rok || !pok {
		return 0, false
	}
	return rv - pv, true
}

func deltaStr(r, p protocol.TrsfConnState, k protocol.TrsfCounterKey) string {
	if d, ok := delta(r, p, k); ok {
		return fmt.Sprintf("%d", d)
	}
	return "-"
}

// parkSummary renders the derived columns the wake counters exist for: how much
// of the interval the run loop spent parked, and what ended those parks. Both
// are undefined without a previous reading, and BLOCK% is also undefined
// without elapsed time, so each returns "-" for ABSENCE — never for a zero,
// which is a measurement and prints as 0%.
//
// The raw counters are not columns. Every one is in --json with its delta,
// because a table that carries them all stops being readable at the width where
// this one is already uncomfortable.
//
// elapsed is measured between the two readings by the ANSWERER's clock, so
// BLOCK% is a share of the interval the counters actually advanced over.
func parkSummary(r, p protocol.TrsfConnState, elapsed time.Duration) (blockPct, wait string) {
	blockPct, wait = "-", "-"
	if blocked, ok := delta(r, p, protocol.TrsfCounterKey_BlockedNs); ok && elapsed > 0 {
		blockPct = fmt.Sprintf("%.0f%%", 100*float64(blocked)/float64(elapsed))
	}
	parks, ok := delta(r, p, protocol.TrsfCounterKey_Blocks)
	if !ok || parks == 0 {
		// No park in the interval: the loop either never stopped or never ran.
		// A share of nothing has no subject, so it is absent rather than zero.
		return blockPct, wait
	}
	timer, _ := delta(r, p, protocol.TrsfCounterKey_WakeTimer)
	send, _ := delta(r, p, protocol.TrsfCounterKey_WakeSend)
	peer := parks - timer - send
	switch {
	case timer >= send && timer >= peer:
		// armed_pacer counts how a park was ARMED, not what ended it, so this
		// is a majority reading rather than an identity: it says most parks in
		// this interval carried the pacer's deadline, and the pacer's own floor
		// is 1 ms.
		pacer, _ := delta(r, p, protocol.TrsfCounterKey_ArmedPacer)
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
		wait = "send/" + dominantPush(r, p)
	default:
		wait = "peer" // an inbound packet, an ACK to send, a window update
	}
	return blockPct, wait
}

// dominantPush names which kind of event pushed the send trigger most over the
// interval. Not a partition of the wakes — these are event counts, and several
// collapse onto one wake — so it answers "what mostly wanted the loop to run",
// which is the question "send" on its own cannot.
func dominantPush(r, p protocol.TrsfConnState) string {
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
		if d, ok := delta(r, p, c.k); ok && d > best {
			best, name = d, c.n
		}
	}
	return name
}

// queueDelay is srtt - min_rtt: how much of the round trip is a queue rather
// than the path. The window's own drain time can BE the srtt, in which case
// cwnd/srtt equals the delivered rate for any cwnd and says nothing; this is
// the reading that separates the two.
func queueDelay(r protocol.TrsfConnState) string {
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

// writeTrsf renders one reading. prev nil means "no previous reading", which is
// the one-shot form; otherwise the delta columns carry the change since it.
//
// prevAt and sampledAt are when the ANSWERER took the two readings, by its own
// clock, and their difference is what BLOCK% is a share of. Both zero in the
// one-shot form, where there is nothing to compare against.
func writeTrsf(out io.Writer, rows []protocol.TrsfConnState, prev map[string]protocol.TrsfConnState, prevAt, sampledAt int64, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		for i := range rows {
			if err := enc.Encode(trsfJSON(rows[i], prev)); err != nil {
				return err
			}
		}
		return nil
	}
	var elapsed time.Duration
	if prevAt != 0 && sampledAt > prevAt {
		elapsed = time.Duration(sampledAt - prevAt)
	}
	const hdr = "%-34s %-7s %-9s %8s %9s %9s %8s %8s %7s %7s %7s %-11s\n"
	fmt.Fprintf(out, hdr, "CID", "ROLE", "TASK", "CWND", "INFLIGHT", "SRTT", "QUEUE",
		"LOSS+", "SPUR+", "LOOP+", "BLOCK%", "WAIT")
	for _, r := range rows {
		task := "-"
		if r.PrincipalTask.Id != ([16]uint8{}) {
			task = hex.EncodeToString(r.PrincipalTask.Id[:])[:8]
		}
		lossD, spurD, loopD := "-", "-", "-"
		blockPct, wait := "-", "-"
		if p, ok := prev[string(r.Cid)]; ok {
			lossD = deltaStr(r, p, protocol.TrsfCounterKey_LossEvents)
			spurD = deltaStr(r, p, protocol.TrsfCounterKey_LossSpurious)
			loopD = deltaStr(r, p, protocol.TrsfCounterKey_LoopIterations)
			blockPct, wait = parkSummary(r, p, elapsed)
		}
		fmt.Fprintf(out, hdr,
			string(r.Cid), r.Role.String(), task,
			fmt.Sprintf("%d", r.CounterOr(protocol.TrsfCounterKey_Cwnd, 0)),
			fmt.Sprintf("%d", r.CounterOr(protocol.TrsfCounterKey_BytesInFlight, 0)),
			(time.Duration(r.CounterOr(protocol.TrsfCounterKey_SrttUs, 0)) * time.Microsecond).String(),
			queueDelay(r),
			lossD, spurD, loopD, blockPct, wait)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "(no connections visible to you)")
	}
	fmt.Fprintln(out)
	return nil
}

// trsfJSON emits every counter the answerer sent, by its own name, plus a delta
// for each when there is something to compare against.
//
// Nothing here enumerates the counters. That is the point of the keyed row: a
// counter added to the transport reaches this output with no edit, so the CLI
// stops being one of the places a new measurement has to be threaded through —
// and a key this build does not know still appears, under the enum's numeric
// fallback name, rather than being silently dropped.
func trsfJSON(r protocol.TrsfConnState, prev map[string]protocol.TrsfConnState) map[string]any {
	m := map[string]any{
		"cid": string(r.Cid), "role": r.Role.String(),
	}
	for _, c := range r.Counters {
		m[c.Key.String()] = c.Value
	}
	if r.PrincipalTask.Id != ([16]uint8{}) {
		m["principal_task"] = hex.EncodeToString(r.PrincipalTask.Id[:])
	}
	// Deltas are emitted only when there IS a previous reading, so a consumer
	// can tell "no change" from "nothing to compare against".
	if p, ok := prev[string(r.Cid)]; ok {
		for _, c := range r.Counters {
			if d, ok := delta(r, p, c.Key); ok {
				m[c.Key.String()+"_delta"] = d
			}
		}
	}
	return m
}
