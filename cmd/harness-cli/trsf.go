package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/agent-harness/cli"
)

// The one-line format for the whole table, header included, so a column added
// to one cannot miss the other.
const trsfHdr = "%-34s %-7s %-9s %8s %9s %9s %8s %8s %7s %7s %7s %-11s\n"

// runTrsf prints congestion state, once or repeatedly.
//
// The repeating form is not a convenience. Several of these counters mean
// nothing as a single sample: loop_iterations separates a run loop that is
// BLOCKED (frozen across two reads) from one that is BUSY-SPINNING (exploding)
// from one that is merely congestion-blocked (advancing slowly), and the loss
// counters only say anything as a rate. So --watch prints the delta, and the
// one-shot form prints the absolute values it has.
//
// The reading itself — the deltas, BLOCK%, WAIT, QUEUE — is derived by
// cli.TrsfSampler, which the TUI modal and the WebUI panel share. This file is
// only the table.
func runTrsf(ctx context.Context, c *cli.Client, runnerCID, watch string, asJSON bool, out io.Writer) error {
	var sampler cli.TrsfSampler
	read := func() error {
		conns, sampledAt, err := c.TrsfStateOn(ctx, runnerCID)
		if err != nil {
			return err
		}
		if asJSON {
			return writeTrsfJSON(out, sampler.ObserveJSON(conns, sampledAt))
		}
		return writeTrsfTable(out, sampler.Observe(conns, sampledAt))
	}
	if watch == "" {
		return read()
	}
	every, err := time.ParseDuration(watch)
	if err != nil {
		return fmt.Errorf("--watch %q: %w", watch, err)
	}
	if every <= 0 {
		return fmt.Errorf("--watch %q: must be positive", watch)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := read(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// writeTrsfTable renders one reading as the table. Every cell arrives already
// rendered, absences included, so nothing here decides what "-" means.
func writeTrsfTable(out io.Writer, rows []cli.TrsfRow) error {
	fmt.Fprintf(out, trsfHdr, "CID", "ROLE", "TASK", "CWND", "INFLIGHT", "SRTT", "QUEUE",
		"LOSS+", "SPUR+", "LOOP+", "BLOCK%", "WAIT")
	for _, r := range rows {
		fmt.Fprintf(out, trsfHdr, r.CID, r.Role, r.Task, r.Cwnd, r.InFlight, r.SRTT,
			r.Queue, r.LossD, r.SpurD, r.LoopD, r.BlockPct, r.Wait)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "(no connections visible to you)")
	}
	fmt.Fprintln(out)
	return nil
}

// writeTrsfJSON emits one object per connection, carrying every counter the
// answerer sent plus its delta. See cli.TrsfSampler.ObserveJSON for why nothing
// enumerates them.
func writeTrsfJSON(out io.Writer, rows []map[string]any) error {
	enc := json.NewEncoder(out)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
