package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func udpRow() *protocol.PortForwardInfo {
	fi := &protocol.PortForwardInfo{
		ForwardId: 7,
		Direction: protocol.PortForwardDirection_Local,
		BindPort:  5353, TargetPort: 5353,
		Protocol: protocol.ForwardProtocol_Udp,
		Route:    protocol.DataPlaneRoute_Splice,
	}
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("127.0.0.1"))
	// The udp group, as the server emits it: a size and the relay's own three
	// causes, together, zeros included.
	fi.SetCounter(protocol.ForwardCounterKey_MaxDatagramSize, 1169)
	fi.SetForwardDropCounters(protocol.ForwardHopRelay, protocol.ForwardDropsBody{})
	return fi
}

func tcpRow() *protocol.PortForwardInfo {
	fi := &protocol.PortForwardInfo{
		ForwardId: 8,
		Direction: protocol.PortForwardDirection_Local,
		BindPort:  3000, TargetPort: 3000,
		Protocol: protocol.ForwardProtocol_Tcp,
		Route:    protocol.DataPlaneRoute_Splice,
	}
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("127.0.0.1"))
	return fi
}

// The spec column round-trips what the operator typed, so a row can be matched
// against the command that made it.
func TestSpecStringCarriesUDP(t *testing.T) {
	if got := PortForwardSpecString(udpRow()); !strings.HasSuffix(got, "/udp") {
		t.Errorf("PortForwardSpecString = %q, want a /udp suffix", got)
	}
}

// A tcp row is UNCHANGED. The axis is opt-in on input and must be opt-in on
// output too, or every existing row's rendering shifts under a reader.
func TestSpecStringLeavesTCPAlone(t *testing.T) {
	got := PortForwardSpecString(tcpRow())
	if strings.Contains(got, "/tcp") || strings.Contains(got, "/udp") {
		t.Errorf("PortForwardSpecString = %q, want no protocol suffix on a tcp row", got)
	}
}

// The datagram numbers appear on a udp row INCLUDING when they are zero.
// "oversize=0" is the answer to "is anything being dropped for size" — the one
// question an operator has when a datagram protocol will not come up through
// the tunnel — and eliding it deletes that answer.
func TestTrafficLineCarriesDatagramCountersIncludingZero(t *testing.T) {
	got := PortForwardTrafficLine(udpRow())
	for _, want := range []string{"mtu=1169", "oversize=0", "congested=0", "queued=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("PortForwardTrafficLine = %q, missing %q", got, want)
		}
	}
}

// A tcp row carries none of them, and the gate is EXISTENCE -- now the key's,
// rather than a protocol test each surface repeats. A tcp forward has no max
// datagram size at all, which is a different statement from one that happens to
// be zero.
func TestTrafficLineOmitsDatagramCountersOnTCP(t *testing.T) {
	got := PortForwardTrafficLine(tcpRow())
	for _, unwanted := range []string{"mtu=", "oversize=", "congested=", "queued="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("PortForwardTrafficLine on a tcp row = %q, must not contain %q", got, unwanted)
		}
	}
}

// protocol and route are on EVERY row; the datagram counters are on the rows
// that HAVE them.
//
// That second half is a deliberate change to the stated contract. It used to be
// "carries everything with no elision", so a tcp row reported
// dropped_oversize=0 -- a count of datagrams for a forward that carries none.
// Absence now means something, and it has to: a key missing from `counters`
// says "this row does not report that", which is how a tcp row says it has no
// datagrams and how a listing that did not ask the endpoints is told apart from
// endpoints that dropped nothing.
func TestJSONCarriesProtocolRouteAndTheCountersARowHas(t *testing.T) {
	for _, row := range []*protocol.PortForwardInfo{udpRow(), tcpRow()} {
		var m map[string]any
		if err := json.Unmarshal([]byte(PortForwardInfoJSONLine(row)), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"protocol", "route"} {
			if _, ok := m[k]; !ok {
				t.Errorf("JSON for protocol=%v is missing %q", row.Protocol, k)
			}
		}
	}

	var udp map[string]any
	_ = json.Unmarshal([]byte(PortForwardInfoJSONLine(udpRow())), &udp)
	if udp["protocol"] != "udp" {
		t.Errorf("protocol = %v, want \"udp\"", udp["protocol"])
	}
	if udp["route"] != "splice" {
		t.Errorf("route = %v, want \"splice\"", udp["route"])
	}
	counters, ok := udp["counters"].(map[string]any)
	if !ok {
		t.Fatalf("a udp row carries no counters object: %v", udp["counters"])
	}
	for _, k := range []string{"max_datagram_size", "relay_dropped_oversize",
		"relay_dropped_congestion", "relay_dropped_queue"} {
		if _, ok := counters[k]; !ok {
			t.Errorf("udp counters missing %q; zeros are answers and must be emitted", k)
		}
	}

	var tcp map[string]any
	_ = json.Unmarshal([]byte(PortForwardInfoJSONLine(tcpRow())), &tcp)
	if _, present := tcp["counters"]; present {
		t.Errorf("a tcp row reported datagram counters: %v", tcp["counters"])
	}
}

// The workspace config value must round-trip the protocol, or a saved and
// re-applied workspace silently turns a udp forward into a tcp one.
func TestConfigSpecRoundTripsProtocol(t *testing.T) {
	spec, ok := PortForwardConfigSpec(udpRow())
	if !ok {
		t.Fatal("PortForwardConfigSpec refused an os-socket udp row")
	}
	if !strings.HasSuffix(spec, "/udp") {
		t.Fatalf("PortForwardConfigSpec = %q, want a /udp suffix", spec)
	}
	// And it must actually parse back to udp, which is the property that
	// matters — the suffix being present is only the visible half of it.
	rest := strings.TrimPrefix(spec, "-L ")
	parsed, err := ParseForwardSpec(rest)
	if err != nil {
		t.Fatalf("ParseForwardSpec(%q): %v", rest, err)
	}
	if parsed.Protocol != protocol.ForwardProtocol_Udp {
		t.Errorf("round-tripped protocol = %v, want udp", parsed.Protocol)
	}
}

func TestConfigSpecLeavesTCPUnsuffixed(t *testing.T) {
	spec, ok := PortForwardConfigSpec(tcpRow())
	if !ok {
		t.Fatal("PortForwardConfigSpec refused an os-socket tcp row")
	}
	if strings.Contains(spec, "/") {
		t.Errorf("PortForwardConfigSpec = %q, want no suffix on a tcp row", spec)
	}
}

// The WebUI reads the same values, and the snapshot row carries counters BOTH
// raw and rendered — raw for computing, rendered so the browser shows the same
// text `forward ls` prints without re-deriving the format in JS.
func TestSnapshotRowCarriesTheNewAxes(t *testing.T) {
	row := ForwardSnapshotRow(udpRow())
	for _, k := range []string{"protocol", "route", "counters", "traffic"} {
		if _, ok := row[k]; !ok {
			t.Errorf("ForwardSnapshotRow is missing %q", k)
		}
	}
	counters, ok := row["counters"].(map[string]any)
	if !ok {
		t.Fatalf("counters is not an object: %v", row["counters"])
	}
	if counters["max_datagram_size"] != float64(1169) {
		t.Errorf("counters[max_datagram_size] = %v, want 1169", counters["max_datagram_size"])
	}
	if row["protocol"] != "udp" {
		t.Errorf("row[protocol] = %v, want \"udp\"", row["protocol"])
	}
}
