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

// parkSummary renders the two derived columns the wake counters exist for:
// how much of the interval the run loop spent parked, and what ended those
// parks. Both are undefined without a previous reading, and BLOCK% is also
// undefined without elapsed time, so each returns "-" for ABSENCE — never for
// a zero, which is a measurement and prints as 0%.
//
// The raw counters are not columns. All ten are in --json with deltas, because
// a table that carries every one of them stops being readable at the width
// where this one is already uncomfortable.
//
// elapsed is measured between the two readings by the ANSWERER's clock, so
// BLOCK% is a share of the interval the counters actually advanced over.
func parkSummary(r, p protocol.TrsfConnState, elapsed time.Duration) (blockPct, wait string) {
	blockPct, wait = "-", "-"
	if elapsed > 0 {
		blockPct = fmt.Sprintf("%.0f%%", 100*float64(r.BlockedNs-p.BlockedNs)/float64(elapsed))
	}
	parks := r.Blocks - p.Blocks
	if parks == 0 {
		// No park in the interval: the loop either never stopped or never ran.
		// A share of nothing has no subject, so it is absent rather than zero.
		return blockPct, wait
	}
	timer := r.WakeTimer - p.WakeTimer
	send := r.WakeSend - p.WakeSend
	peer := parks - timer - send
	switch {
	case timer >= send && timer >= peer:
		// armed_pacer counts how a park was ARMED, not what ended it, so this
		// is a majority reading rather than an identity: it says most parks in
		// this interval carried the pacer's deadline, and the pacer's own floor
		// is 1 ms.
		if r.ArmedPacer-p.ArmedPacer > parks/2 {
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
		d uint64
	}{
		{"app", r.SendPushApp - p.SendPushApp},    // waiting on its caller
		{"ack", r.SendPushAck - p.SendPushAck},    // the window was the constraint
		{"self", r.SendPushSelf - p.SendPushSelf}, // cycling, not waiting
		{"cwnd", r.SendPushCwnd - p.SendPushCwnd}, // congestion-blocked, revived
		{"loss", r.SendPushLoss - p.SendPushLoss}, // retransmission pressure
		{"other", r.SendPushOther - p.SendPushOther},
	} {
		if c.d > best {
			best, name = c.d, c.n
		}
	}
	return name
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
	fmt.Fprintf(out, "%-34s %-7s %-9s %8s %9s %9s %8s %7s %7s %7s %-11s\n",
		"CID", "ROLE", "TASK", "CWND", "INFLIGHT", "SRTT", "LOSS+", "SPUR+", "LOOP+", "BLOCK%", "WAIT")
	for _, r := range rows {
		task := "-"
		if r.PrincipalTask.Id != ([16]uint8{}) {
			task = hex.EncodeToString(r.PrincipalTask.Id[:])[:8]
		}
		lossD, spurD, loopD := "-", "-", "-"
		blockPct, wait := "-", "-"
		if p, ok := prev[string(r.Cid)]; ok {
			lossD = fmt.Sprintf("%d", r.LossEvents-p.LossEvents)
			spurD = fmt.Sprintf("%d", r.LossSpurious-p.LossSpurious)
			loopD = fmt.Sprintf("%d", r.LoopIterations-p.LoopIterations)
			blockPct, wait = parkSummary(r, p, elapsed)
		}
		fmt.Fprintf(out, "%-34s %-7s %-9s %8d %9d %9s %8s %7s %7s %7s %-11s\n",
			string(r.Cid), r.Role.String(), task,
			r.Cwnd, r.BytesInFlight,
			(time.Duration(r.SrttUs) * time.Microsecond).String(),
			lossD, spurD, loopD, blockPct, wait)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "(no connections visible to you)")
	}
	fmt.Fprintln(out)
	return nil
}

func trsfJSON(r protocol.TrsfConnState, prev map[string]protocol.TrsfConnState) map[string]any {
	m := map[string]any{
		"cid": string(r.Cid), "role": r.Role.String(),
		"cwnd": r.Cwnd, "bytes_in_flight": r.BytesInFlight,
		"srtt_us": r.SrttUs, "rttvar_us": r.RttvarUs, "mtu": r.Mtu,
		"send_queue": r.SendQueue, "recv_queue": r.RecvQueue,
		"send_streams": r.SendStreams, "recv_streams": r.RecvStreams,
		"loop_iterations": r.LoopIterations,
		"loss_events":     r.LossEvents, "loss_packets": r.LossPackets,
		"loss_spurious": r.LossSpurious,
		// The run loop's account of its own waiting. All five raw, because the
		// table shows only the two derived readings and this is where a
		// consumer that wants the split gets it.
		"blocked_ns": r.BlockedNs, "blocks": r.Blocks,
		"wake_timer": r.WakeTimer, "wake_send": r.WakeSend,
		"armed_pacer": r.ArmedPacer,
		// Why the send trigger was pushed. EVENT counts, so these do not sum
		// to blocks and are not a partition of the wakes.
		"send_push_app": r.SendPushApp, "send_push_ack": r.SendPushAck,
		"send_push_self": r.SendPushSelf, "send_push_cwnd": r.SendPushCwnd,
		"send_push_loss": r.SendPushLoss, "send_push_other": r.SendPushOther,
	}
	if r.PrincipalTask.Id != ([16]uint8{}) {
		m["principal_task"] = hex.EncodeToString(r.PrincipalTask.Id[:])
	}
	// Deltas are emitted only when there IS a previous reading, so a consumer
	// can tell "no change" from "nothing to compare against".
	if p, ok := prev[string(r.Cid)]; ok {
		m["loss_events_delta"] = r.LossEvents - p.LossEvents
		m["loss_spurious_delta"] = r.LossSpurious - p.LossSpurious
		m["loop_iterations_delta"] = r.LoopIterations - p.LoopIterations
		m["blocked_ns_delta"] = r.BlockedNs - p.BlockedNs
		m["blocks_delta"] = r.Blocks - p.Blocks
		m["wake_timer_delta"] = r.WakeTimer - p.WakeTimer
		m["wake_send_delta"] = r.WakeSend - p.WakeSend
		m["armed_pacer_delta"] = r.ArmedPacer - p.ArmedPacer
		m["send_push_app_delta"] = r.SendPushApp - p.SendPushApp
		m["send_push_ack_delta"] = r.SendPushAck - p.SendPushAck
		m["send_push_self_delta"] = r.SendPushSelf - p.SendPushSelf
		m["send_push_cwnd_delta"] = r.SendPushCwnd - p.SendPushCwnd
		m["send_push_loss_delta"] = r.SendPushLoss - p.SendPushLoss
		m["send_push_other_delta"] = r.SendPushOther - p.SendPushOther
	}
	return m
}
