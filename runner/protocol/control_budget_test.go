package protocol

import (
	"fmt"
	"strings"
	"testing"
)

// TestPskAuthRequestRunnerAuthFitsDatagram encodes the runner's first message
// as wide as this fleet makes it: --agents all (12 profiles), several bundled
// roots, a long hostname, plus the 96 bytes RunnerAuth adds. It must still be
// one udp datagram, because the handshake is a control message too.
func TestPskAuthRequestRunnerAuthFitsDatagram(t *testing.T) {
	var rh RunnerHello
	rh.Version = 1
	rh.MaxTasks = 8
	rh.SetHostname([]byte(strings.Repeat("h", 63)))
	rh.SetGoos([]byte("windows"))
	rh.SetAgentBin([]byte("claude"))
	var roots []AllowedRoot
	for i := 0; i < 4; i++ {
		var r AllowedRoot
		r.SetPath([]byte(fmt.Sprintf("/home/someone/workspace/a-fairly-long-repository-name-%02d", i)))
		roots = append(roots, r)
	}
	rh.SetAllowedRoots(roots)
	var profiles []AgentProfileName
	for i := 0; i < 12; i++ {
		var p AgentProfileName
		p.SetName([]byte(fmt.Sprintf("sandbox-agent-%02d", i)))
		profiles = append(profiles, p)
	}
	rh.SetAgentProfiles(profiles)

	auth := RunnerAuth{Hello: rh}
	var req PskAuthRequest
	req.Role = AuthRole_Runner
	req.SetBinder(nil)
	if !req.SetRunnerAuth(auth) {
		t.Fatal("SetRunnerAuth returned false")
	}
	buf, err := req.Append([]byte{0x45})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckControlMessage("udp", 1170, len(buf)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckControlMessage(t *testing.T) {
	err := CheckControlMessage("udp", 1170, 1171)
	if err == nil || !strings.Contains(err.Error(), "1171") || !strings.Contains(err.Error(), "1170") {
		t.Fatalf("over budget on udp: %v, want an error naming 1171 and 1170", err)
	}
	for _, c := range []struct {
		tr        string
		budget, n int
	}{{"udp", 1170, 1170}, {"udp", 1470, 1300}, {"ws", 1170, 64 << 10}, {"wss", 1170, 64 << 10}} {
		if err := CheckControlMessage(c.tr, c.budget, c.n); err != nil {
			t.Errorf("%s budget=%d n=%d: %v, want nil", c.tr, c.budget, c.n, err)
		}
	}
}
