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
		rows, err := c.TrsfStateOn(ctx, runnerCID)
		if err != nil {
			return err
		}
		return writeTrsf(out, rows, nil, asJSON)
	}
	every, err := time.ParseDuration(watch)
	if err != nil {
		return fmt.Errorf("--watch %q: %w", watch, err)
	}
	if every <= 0 {
		return fmt.Errorf("--watch %q: must be positive", watch)
	}
	prev := map[string]protocol.TrsfConnState{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rows, err := c.TrsfStateOn(ctx, runnerCID)
		if err != nil {
			return err
		}
		if err := writeTrsf(out, rows, prev, asJSON); err != nil {
			return err
		}
		for _, r := range rows {
			prev[string(r.Cid)] = r
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// writeTrsf renders one reading. prev nil means "no previous reading", which is
// the one-shot form; otherwise the delta columns carry the change since it.
func writeTrsf(out io.Writer, rows []protocol.TrsfConnState, prev map[string]protocol.TrsfConnState, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		for i := range rows {
			if err := enc.Encode(trsfJSON(rows[i], prev)); err != nil {
				return err
			}
		}
		return nil
	}
	fmt.Fprintf(out, "%-34s %-7s %-9s %8s %9s %9s %8s %7s %7s\n",
		"CID", "ROLE", "TASK", "CWND", "INFLIGHT", "SRTT", "LOSS+", "SPUR+", "LOOP+")
	for _, r := range rows {
		task := "-"
		if r.PrincipalTask.Id != ([16]uint8{}) {
			task = hex.EncodeToString(r.PrincipalTask.Id[:])[:8]
		}
		lossD, spurD, loopD := "-", "-", "-"
		if p, ok := prev[string(r.Cid)]; ok {
			lossD = fmt.Sprintf("%d", r.LossEvents-p.LossEvents)
			spurD = fmt.Sprintf("%d", r.LossSpurious-p.LossSpurious)
			loopD = fmt.Sprintf("%d", r.LoopIterations-p.LoopIterations)
		}
		fmt.Fprintf(out, "%-34s %-7s %-9s %8d %9d %9s %8s %7s %7s\n",
			string(r.Cid), r.Role.String(), task,
			r.Cwnd, r.BytesInFlight,
			(time.Duration(r.SrttUs) * time.Microsecond).String(),
			lossD, spurD, loopD)
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
	}
	return m
}
