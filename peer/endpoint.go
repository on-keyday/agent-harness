package peer

import (
	"time"

	"github.com/on-keyday/objtrsf/objproto"
)

// StartEndpointMaintenance starts the two sweepers an objproto.Endpoint needs
// for as long as it exists, and is the one place their intervals are written.
//
// # An Endpoint is PROCESS-SCOPED. Dial many Connections on one.
//
// An Endpoint is not a connection: it owns the socket and the map of every
// connection reachable through it (`GetConnection`, `ListActiveConnections`,
// `SetProxy`), which is why Dial takes one as a parameter rather than making
// it, and why both sweepers below iterate the whole set. Close belongs to the
// Connection — `peer.Conn.Close()` — and the Endpoint interface deliberately
// has none: it is meant to outlive every connection made on it.
//
// Building one per dial therefore does not merely leak; it dissolves the thing
// the type is for. The observable consequences, all measured 2026-09-11 on a
// runner reconnecting in a loop: four goroutines per attempt that never exit
// (these two plus the transport's own pair), one bound UDP socket per attempt,
// a sweeper per endpoint walking a one-entry map — and a source port that
// changes on every reconnect, which is exactly what `cli/dataplane_dial.go`
// says the relay and the NAT punch must be able to rely on NOT changing.
//
// That rule was the original godoc ("process-scoped Endpoint は走り続ける").
// It was lost in `d75ba5a1`, where a review found the doc no longer matched a
// refactor that had moved construction into the dial path — and the doc was
// rewritten to match the code instead of the other way round. Two weeks later
// `--persist` put that dial path inside a retry loop.
func StartEndpointMaintenance(ep objproto.Endpoint) {
	// Interval, then the three ages: a handshake with no answer, a connection
	// with no traffic, a proxy entry nobody redeemed. The connection age is the
	// one with a visible cost — it is what ends a connection whose path died
	// silently, since neither a withdrawn route nor a blackhole makes a send
	// fail, so nothing else notices. Measured ~65s to fail over at 1 minute.
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
	go objproto.AutoKeyUpdate(ep, 1*time.Minute, objproto.DefaultKeyUpdateInterval)
}
