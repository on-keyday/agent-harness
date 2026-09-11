package runner

import (
	"strings"
	"testing"
)

func TestServerCandidatesSchemes(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want []string
	}{
		{"ws:a:1-*", []string{"ws"}},
		{"udp:a:1-*", []string{"udp"}},
		// Distinct, in order of first appearance: the order is what decides
		// which leg a single-transport list builds.
		{"ws:a:1-*,ws:b:2-*", []string{"ws"}},
		{"udp:a:1-*,ws:b:2-*", []string{"udp", "ws"}},
		{"ws:a:1-*,udp:b:2-*,ws:c:3-*", []string{"ws", "udp"}},
		{"wss:a:1-*,udp:b:2-*", []string{"wss", "udp"}},
		// Not the endpoint's problem: an unrecognised transport is reported as
		// itself and fails when that candidate is dialed.
		{"quic:a:1-*", []string{"quic"}},
		// No ':' at all: Cut yields the whole string, which is reported as-is.
		// Harmless — nothing recognises it as a leg, so it only ever appears in
		// the "no dialable transport in [...]" error, where echoing what the
		// operator wrote is what makes the message usable.
		{"nonsense", []string{"nonsense"}},
	} {
		c, err := ParseServerCandidates(tc.spec)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.spec, err)
		}
		got := c.Schemes()
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Schemes(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// A list that spans ws and udp needs ONE endpoint carrying both legs — the
// construct runner/listen.go already uses for the same job (a single runner
// process reachable over both transports). Two single-leg endpoints would leave
// whichever one loses the walk with a bound socket, a GC goroutine pair and an
// accept channel nobody reads.
func TestEndpointLegsFor(t *testing.T) {
	for _, tc := range []struct {
		schemes  []string
		wantWS   bool
		wantUDP  bool
		wantErr  bool
		errNames string
	}{
		{schemes: []string{"ws"}, wantWS: true},
		{schemes: []string{"wss"}, wantWS: true},
		{schemes: []string{"udp"}, wantUDP: true},
		{schemes: []string{"ws", "udp"}, wantWS: true, wantUDP: true},
		{schemes: []string{"udp", "wss"}, wantWS: true, wantUDP: true},
		// ws and wss share one leg: the CID's scheme picks TLS at dial time.
		{schemes: []string{"ws", "wss"}, wantWS: true},
		// An unrecognised transport does NOT sink the whole list: a typo in the
		// last candidate must not stop the runner reaching the first one.
		{schemes: []string{"ws", "quic"}, wantWS: true},
		// ... but a list with nothing dialable in it has no endpoint to build,
		// and the error names what it found.
		{schemes: []string{"quic"}, wantErr: true, errNames: "quic"},
		{schemes: []string{""}, wantErr: true},
	} {
		legs, err := endpointLegsFor(tc.schemes)
		if tc.wantErr {
			if err == nil {
				t.Errorf("endpointLegsFor(%v): want error, got %+v", tc.schemes, legs)
				continue
			}
			if tc.errNames != "" && !strings.Contains(err.Error(), tc.errNames) {
				t.Errorf("endpointLegsFor(%v) error %q does not name %q", tc.schemes, err, tc.errNames)
			}
			continue
		}
		if err != nil {
			t.Errorf("endpointLegsFor(%v): %v", tc.schemes, err)
			continue
		}
		if legs.ws != tc.wantWS || legs.udp != tc.wantUDP {
			t.Errorf("endpointLegsFor(%v) = {ws:%v udp:%v}, want {ws:%v udp:%v}",
				tc.schemes, legs.ws, legs.udp, tc.wantWS, tc.wantUDP)
		}
	}
}
