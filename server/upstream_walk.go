package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// hopErr holds per-hop failure details returned by walkAndDispatchUpstreamHops.
type hopErr struct {
	HopID     string
	Status    protocol.EstablishRelayStatus
	Err       error
}

// walkAndDispatchUpstreamHops walks entry.Via.Via... and dispatches
// EstablishRelay{slotID, target=H_downstream.ViaDialAddr} to each
// upstream hop in parallel. Returns (allOk, walkErr, hopErrs).
//
// walkErr is non-nil only on loop detection. The caller maps it to its
// own status code: ChainUnwalkable for the chained-relay runtime path,
// ViaRelayFailed for the registration path.
//
// hopErrs is non-nil when at least one hop returned a non-Ok response;
// allOk is false in that case. Per-hop error context (HopID, Status,
// Err) is preserved for the caller's log discipline — this function
// does NOT log anything itself.
//
// When entry.Via == nil the function returns (true, nil, nil) immediately;
// no dispatch is performed.
//
// timeout specifies a deadline for all hop dispatches. Zero uses a 10 s
// default. When the caller already holds a tighter context deadline (e.g.
// HandleWithVia's relayCtx), the outer deadline still wins because the
// inner context is derived from ctx.
func walkAndDispatchUpstreamHops(
	ctx context.Context,
	entry *RunnerEntry,
	slotID uint16,
	timeout time.Duration,
	send func(context.Context, *RunnerEntry, protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error),
	_ *slog.Logger, // reserved for future structured logging; callers log hopErrs themselves
) (allOk bool, walkErr error, hopErrs []hopErr) {
	// No chain — caller handles the Direct / no-op case.
	if entry.Via == nil {
		return true, nil, nil
	}

	type hopSetup struct {
		hop             *RunnerEntry
		downViaDialAddr protocol.RunnerID
	}

	var hops []hopSetup
	cur := entry
	seen := map[string]struct{}{entry.ID: {}}

	for cur.Via != nil {
		if _, dup := seen[cur.Via.ID]; dup {
			return false, fmt.Errorf("loop detected in Via chain at hop %q", cur.Via.ID), nil
		}
		hops = append(hops, hopSetup{
			hop:             cur.Via,
			downViaDialAddr: protocol.ConnIDToRunnerID(cur.ViaDialAddr),
		})
		seen[cur.Via.ID] = struct{}{}
		cur = cur.Via
	}

	if timeout == 0 {
		timeout = 10 * time.Second
	}
	hopCtx, hopCancel := context.WithTimeout(ctx, timeout)
	defer hopCancel()

	type result struct {
		ok        bool
		err       error
		hopID     string
		hopStatus protocol.EstablishRelayStatus
	}
	results := make(chan result, len(hops))
	for _, hp := range hops {
		hp := hp
		go func() {
			req := protocol.EstablishRelayRequest{
				Target: hp.downViaDialAddr,
				SlotId: slotID,
			}
			resp, err := send(hopCtx, hp.hop, req)
			results <- result{
				ok:        err == nil && resp.Status == protocol.EstablishRelayStatus_Ok,
				err:       err,
				hopID:     hp.hop.ID,
				hopStatus: resp.Status,
			}
		}()
	}

	allOk = true
	for i := 0; i < len(hops); i++ {
		r := <-results
		if !r.ok {
			allOk = false
			hopErrs = append(hopErrs, hopErr{
				HopID:  r.hopID,
				Status: r.hopStatus,
				Err:    r.err,
			})
		}
	}
	return allOk, nil, hopErrs
}
