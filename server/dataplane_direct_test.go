package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A direct dial needs both ends on a transport a client can actually open a
// socket to, and on the SAME one: there is no forwarder in the middle to
// translate. A browser on WebSocket cannot dial a runner at all, so for those
// the relay stays the only end-to-end route there is.
func TestDataPlaneDirectNeedsBothEndsOnUDP(t *testing.T) {
	if !dataPlaneDirectOK(cid("udp", 1, 10), cid("udp", 2, 11)) {
		t.Fatal("udp/udp should be dialable directly")
	}
	for _, p := range [][2]string{{"ws", "udp"}, {"udp", "ws"}, {"ws", "ws"}, {"", "udp"}, {"udp", ""}} {
		if dataPlaneDirectOK(cid(p[0], 1, 10), cid(p[1], 2, 11)) {
			t.Fatalf("%s/%s must not be offered a direct dial", p[0], p[1])
		}
	}
}

// runner_cid means WHERE TO DIAL, so the relayed route must leave it zero.
// Naming the runner there without having punched toward the client would send
// the client at an address a firewall drops -- the failure the punch exists to
// prevent, arrived at by saying too much.
func TestRelayedRouteNamesNoRunnerAddress(t *testing.T) {
	var zero protocol.ConnID
	if zero.TransportLen != 0 {
		t.Fatal("the zero ConnID is supposed to encode as absent")
	}
	// The client's branch is keyed on exactly this, so pin the encoding both
	// ways round: absent stays absent across the wire.
	b, err := zero.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back protocol.ConnID
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.TransportLen != 0 {
		t.Fatalf("an absent runner_cid came back present: %d", back.TransportLen)
	}
}
