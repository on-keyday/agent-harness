package runner

import (
	"context"
	"net/netip"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// probeSender is the slice of objproto.Endpoint that punchToward needs, so the
// loop is testable without a socket. objproto.Endpoint satisfies it.
type probeSender interface {
	SendProbe(cid objproto.ConnectionID, macAddr [6]byte, ipAddr netip.AddrPort) error
}

// punchToward opens the return path for a peer that is about to dial this
// runner directly.
//
// A host firewall drops an unsolicited inbound datagram even on a LAN with no
// NAT in it. That is measured, not assumed: three live runners refused a dial
// that then completed in 33 ms once this loop had run, with an un-punched
// control timing out in the same run from the same client socket
// (docs/superpowers/specs/2026-09-06-direct-client-runner-dial-probe.md, probes
// 2 and 3). Sending from THIS socket toward the exact address and port the peer
// will dial from is what opens the mapping, so target must name that socket and
// not merely that host.
//
// Nothing calls this with a set target yet: the server writes transport_len == 0
// and the loop returns immediately. It ships now so that adding the direct path
// later is a change to the server and the client, with no runner to redeploy —
// a .bgn change costs a coordinated restart across every runner host, and this
// is how that bill is paid once instead of twice.
//
// Returns how many probes were sent.
func punchToward(ctx context.Context, ep probeSender, target protocol.RunnerID, interval time.Duration) int {
	if target.TransportLen == 0 {
		return 0
	}
	cid := protocol.RunnerIDToConnID(target)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sent := 0
	for {
		// A send error is not fatal: the mapping this is trying to open may
		// simply not be reachable yet, and the next tick retries.
		if err := ep.SendProbe(cid, [6]byte{}, cid.Addr); err == nil {
			sent++
		}
		select {
		case <-ctx.Done():
			return sent
		case <-ticker.C:
		}
	}
}
