package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ChainedRelayHandler handles RunnerMessage{RequestChainedRelay} from a runner.
//
// When a runner L was registered via Phase C (L.Via != nil), L sends this
// request before its own local SetProxy so that every intermediate hop in the
// Via chain gets an EstablishRelayRequest and sets up its own proxySettings
// entry. Server walks L.Via.Via... in parallel and replies with
// ChainedRelayResponse once all hops acknowledge (or any hop fails).
type ChainedRelayHandler struct {
	Logger   *slog.Logger
	Registry *Registry

	// SendEstablishRelay dispatches an EstablishRelayRequest to the given
	// runner entry and returns the response. Wired to
	// Server.sendEstablishRelayRequest in production.
	SendEstablishRelay func(ctx context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error)

	// HopTimeout is the per-hop timeout for EstablishRelay dispatches.
	// Zero → 10s default.
	HopTimeout time.Duration

	// inFlight is the set of runner conn CID strings that have an in-progress
	// RequestChainedRelay. Keyed by conn.ConnectionID().String().
	inFlight   map[string]struct{}
	inFlightMu sync.Mutex
}

// Handle processes a RequestChainedRelay from runner L (identified by conn).
// It returns a ChainedRelayResponse that the caller should send back to L.
//
// The method is concurrency-safe. Concurrent calls from the SAME runner conn
// are rejected with AnotherInFlight; concurrent calls from different runners
// are handled independently.
func (h *ChainedRelayHandler) Handle(
	ctx context.Context,
	conn ConnHandle,
	req protocol.RequestChainedRelay,
) protocol.ChainedRelayResponse {
	runnerID := conn.ConnectionID().String()

	// --- In-flight guard ---
	h.inFlightMu.Lock()
	if h.inFlight == nil {
		h.inFlight = make(map[string]struct{})
	}
	if _, exists := h.inFlight[runnerID]; exists {
		h.inFlightMu.Unlock()
		return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_AnotherInFlight}
	}
	h.inFlight[runnerID] = struct{}{}
	h.inFlightMu.Unlock()
	defer func() {
		h.inFlightMu.Lock()
		delete(h.inFlight, runnerID)
		h.inFlightMu.Unlock()
	}()

	// --- 1. Look up requesting runner L ---
	entry, ok := h.Registry.Get(runnerID)
	if !ok {
		h.Logger.Warn("chained-relay: requester not in registry", "runner", runnerID)
		return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_ChainUnwalkable}
	}

	// --- 2. Direct runner (no chain) ---
	if entry.Via == nil {
		return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Direct}
	}

	// --- 3. Walk Via chain, collecting (hop, downstream.ViaDialAddr) pairs ---
	// Each iteration: cur is the downstream entry whose ViaDialAddr is the
	// address cur.Via (the hop) will use for its SetProxy.allocate.
	type hopSetup struct {
		hop             *RunnerEntry
		downViaDialAddr objproto.ConnectionID
	}

	var hops []hopSetup
	// Start walk from L itself. cur.Via is the first intermediate hop;
	// cur.ViaDialAddr is what that hop needs for SetProxy.allocate.
	cur := &entry
	seen := map[string]struct{}{entry.ID: {}}

	for cur.Via != nil {
		if _, dup := seen[cur.Via.ID]; dup {
			// Loop detected.
			h.Logger.Warn("chained-relay: loop detected in Via chain",
				"runner", runnerID,
				"loop_at", cur.Via.ID)
			return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_ChainUnwalkable}
		}
		hops = append(hops, hopSetup{
			hop:             cur.Via,
			downViaDialAddr: cur.ViaDialAddr,
		})
		seen[cur.Via.ID] = struct{}{}
		cur = cur.Via
	}

	// --- 4. Dispatch EstablishRelayRequest to all hops in parallel ---
	timeout := h.HopTimeout
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
			establishReq := protocol.EstablishRelayRequest{
				Target: protocol.ConnIDToRunnerID(hp.downViaDialAddr),
				SlotId: req.SlotId,
			}
			resp, err := h.SendEstablishRelay(hopCtx, hp.hop, establishReq)
			results <- result{
				ok:        err == nil && resp.Status == protocol.EstablishRelayStatus_Ok,
				err:       err,
				hopID:     hp.hop.ID,
				hopStatus: resp.Status,
			}
		}()
	}

	// --- 5. Collect all responses ---
	allOk := true
	for i := 0; i < len(hops); i++ {
		r := <-results
		if !r.ok {
			allOk = false
			h.Logger.Warn("chained-relay: hop setup failed",
				"runner", runnerID, "slotId", req.SlotId,
				"hop", r.hopID, "hopStatus", r.hopStatus, "err", r.err)
		}
	}

	if !allOk {
		return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_HopSetupFailed}
	}
	return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Ok}
}
