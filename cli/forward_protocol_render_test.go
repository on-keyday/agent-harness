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
		Protocol:        protocol.ForwardProtocol_Udp,
		Route:           protocol.DataPlaneRoute_Splice,
		MaxDatagramSize: 1169,
	}
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("127.0.0.1"))
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

// A tcp row carries none of them, and the gate is EXISTENCE (protocol == udp),
// never the values: a tcp forward has no max datagram size at all, which is a
// different statement from one that happens to be zero.
func TestTrafficLineOmitsDatagramCountersOnTCP(t *testing.T) {
	got := PortForwardTrafficLine(tcpRow())
	for _, unwanted := range []string{"mtu=", "oversize=", "congested=", "queued="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("PortForwardTrafficLine on a tcp row = %q, must not contain %q", got, unwanted)
		}
	}
}

// The JSON form carries everything with no elision, which is its stated
// contract. protocol and route are on EVERY row; the datagram numbers too,
// because a consumer scripting against JSON reads the key's presence as the
// schema and its absence as a different shape.
func TestJSONCarriesProtocolRouteAndDatagramFields(t *testing.T) {
	for _, row := range []*protocol.PortForwardInfo{udpRow(), tcpRow()} {
		var m map[string]any
		if err := json.Unmarshal([]byte(PortForwardInfoJSONLine(row)), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"protocol", "route", "max_datagram_size",
			"dropped_oversize", "dropped_congestion", "dropped_queue"} {
			if _, ok := m[k]; !ok {
				t.Errorf("JSON for protocol=%v is missing %q", row.Protocol, k)
			}
		}
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(PortForwardInfoJSONLine(udpRow())), &m)
	if m["protocol"] != "udp" {
		t.Errorf("protocol = %v, want \"udp\"", m["protocol"])
	}
	if m["route"] != "splice" {
		t.Errorf("route = %v, want \"splice\"", m["route"])
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
	for _, k := range []string{"protocol", "route", "max_datagram_size",
		"dropped_oversize", "dropped_congestion", "dropped_queue", "traffic"} {
		if _, ok := row[k]; !ok {
			t.Errorf("ForwardSnapshotRow is missing %q", k)
		}
	}
	if row["protocol"] != "udp" {
		t.Errorf("row[protocol] = %v, want \"udp\"", row["protocol"])
	}
}
