//go:build !js

package sshgw

import "testing"

// directTcpipPayload builds an RFC 4254 §7.2 channel-open payload.
func directTcpipPayload(host string, port uint32, originAddr string, originPort uint32) []byte {
	b := appendU32(nil, uint32(len(host)))
	b = append(b, host...)
	b = appendU32(b, port)
	b = appendU32(b, uint32(len(originAddr)))
	b = append(b, originAddr...)
	b = appendU32(b, originPort)
	return b
}

func TestParseDirectTCPIP(t *testing.T) {
	host, port, err := parseDirectTCPIP(directTcpipPayload("127.0.0.1", 22, "192.0.2.7", 51234))
	if err != nil {
		t.Fatalf("parseDirectTCPIP: %v", err)
	}
	if host != "127.0.0.1" || port != 22 {
		t.Errorf("got %s:%d, want 127.0.0.1:22", host, port)
	}
}

// The payload carries TWO host/port pairs and only the first is the target.
// Reading the originator by mistake dials the ssh client's own machine, which
// on a loopback-bound gateway is a plausible-looking address that connects to
// the wrong host — so the two are made different here and the target asserted
// by value, not by "some host came back".
func TestParseDirectTCPIP_OriginatorIsNotTheTarget(t *testing.T) {
	host, port, err := parseDirectTCPIP(directTcpipPayload("build-host", 8080, "10.0.0.5", 40000))
	if err != nil {
		t.Fatalf("parseDirectTCPIP: %v", err)
	}
	if host != "build-host" {
		t.Errorf("host = %q, want build-host (the originator address was read as the target)", host)
	}
	if port != 8080 {
		t.Errorf("port = %d, want 8080 (the originator port was read as the target)", port)
	}
}

func TestParseDirectTCPIP_Rejects(t *testing.T) {
	// host and port present, the originator pair absent: a payload that stops
	// exactly where a reader interested only in the target would stop.
	noOriginator := appendU32(nil, 1)
	noOriginator = append(noOriginator, 'h')
	noOriginator = appendU32(noOriginator, 22)

	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty payload", nil},
		{"empty host", directTcpipPayload("", 22, "10.0.0.5", 40000)},
		{"port zero", directTcpipPayload("h", 0, "10.0.0.5", 40000)},
		{"port out of range", directTcpipPayload("h", 65536, "10.0.0.5", 40000)},
		// A length prefix that claims more than the payload holds is the shape a
		// hand-rolled reader turns into a panic; the generated decoder checks it.
		{"host length exceeds payload", []byte{0, 0, 0, 9, 'a'}},
		{"originator truncated", noOriginator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseDirectTCPIP(tc.payload); err == nil {
				t.Errorf("want an error for %s", tc.name)
			}
		})
	}
}
