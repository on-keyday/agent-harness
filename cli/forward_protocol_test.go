package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Every spelling that parsed before the protocol axis existed still parses, and
// still means tcp. This is the compatibility half of the suffix design: the
// axis is opt-in, so no workspace file and no operator's muscle memory changes.
func TestForwardSpecWithoutSuffixIsTCP(t *testing.T) {
	for _, s := range []string{"3000", "3000:127.0.0.1:3000", "0.0.0.0:3000:db.internal:5432"} {
		got, err := ParseForwardSpec(s)
		if err != nil {
			t.Fatalf("ParseForwardSpec(%q): %v", s, err)
		}
		if got.Protocol != protocol.ForwardProtocol_Tcp {
			t.Errorf("ParseForwardSpec(%q).Protocol = %v, want tcp", s, got.Protocol)
		}
	}
	for _, s := range []string{"3000", "3000:127.0.0.1:3000", "0.0.0.0:3000:db.internal:5432"} {
		got, err := ParseRemoteForwardSpec(s)
		if err != nil {
			t.Fatalf("ParseRemoteForwardSpec(%q): %v", s, err)
		}
		if got.Protocol != protocol.ForwardProtocol_Tcp {
			t.Errorf("ParseRemoteForwardSpec(%q).Protocol = %v, want tcp", s, got.Protocol)
		}
	}
}

// The suffix attaches to the whole spec, in every form the grammar has --
// including the bare port, whose expansion has to strip it before splitting.
func TestForwardSpecUDPSuffix(t *testing.T) {
	cases := []struct {
		in         string
		bind       string
		localPort  int
		remoteHost string
		remotePort int
	}{
		{"5353/udp", "127.0.0.1", 5353, "127.0.0.1", 5353},
		{"5353:127.0.0.1:5353/udp", "127.0.0.1", 5353, "127.0.0.1", 5353},
		{"0.0.0.0:5353:dns.internal:53/udp", "0.0.0.0", 5353, "dns.internal", 53},
	}
	for _, c := range cases {
		got, err := ParseForwardSpec(c.in)
		if err != nil {
			t.Fatalf("ParseForwardSpec(%q): %v", c.in, err)
		}
		if got.Protocol != protocol.ForwardProtocol_Udp {
			t.Errorf("ParseForwardSpec(%q).Protocol = %v, want udp", c.in, got.Protocol)
		}
		if got.BindAddr != c.bind || got.LocalPort != c.localPort ||
			got.RemoteHost != c.remoteHost || got.RemotePort != c.remotePort {
			t.Errorf("ParseForwardSpec(%q) = %+v, want bind=%s lport=%d rhost=%s rport=%d",
				c.in, got, c.bind, c.localPort, c.remoteHost, c.remotePort)
		}
	}
}

// -R takes the same suffix, and a udp -R dials udp locally. DialNetwork is a
// DIFFERENT axis -- it is the socket the client opens for its own target, which
// X11 sets to "unix" -- so a test pins that udp reaches it rather than leaving
// the client dialling tcp against a udp forward.
func TestRemoteForwardSpecUDPSuffix(t *testing.T) {
	got, err := ParseRemoteForwardSpec("5353:127.0.0.1:5353/udp")
	if err != nil {
		t.Fatalf("ParseRemoteForwardSpec: %v", err)
	}
	if got.Protocol != protocol.ForwardProtocol_Udp {
		t.Errorf("Protocol = %v, want udp", got.Protocol)
	}
	if got.DialNetwork != "udp" {
		t.Errorf("DialNetwork = %q, want \"udp\": a udp forward whose client dials tcp reaches nothing", got.DialNetwork)
	}
}

// An explicit /tcp is accepted, so an operator who writes the axis out is not
// told it does not exist.
func TestForwardSpecExplicitTCPSuffix(t *testing.T) {
	got, err := ParseForwardSpec("3000:127.0.0.1:3000/tcp")
	if err != nil {
		t.Fatalf("ParseForwardSpec: %v", err)
	}
	if got.Protocol != protocol.ForwardProtocol_Tcp {
		t.Errorf("Protocol = %v, want tcp", got.Protocol)
	}
	if got.LocalPort != 3000 || got.RemotePort != 3000 {
		t.Errorf("got %+v, want ports 3000/3000", got)
	}
}

// An unknown suffix names what it accepts. "forward: bad spec" alone would send
// the reader to the colon grammar, which is not where the mistake is.
func TestForwardSpecUnknownSuffixIsRejected(t *testing.T) {
	for _, s := range []string{"3000/sctp", "3000/", "3000:127.0.0.1:3000/UDP "} {
		_, err := ParseForwardSpec(s)
		if err == nil {
			t.Fatalf("ParseForwardSpec(%q) accepted an unknown protocol suffix", s)
		}
		if !strings.Contains(err.Error(), "tcp") || !strings.Contains(err.Error(), "udp") {
			t.Errorf("ParseForwardSpec(%q) error %q does not name the accepted suffixes", s, err)
		}
	}
	if _, err := ParseRemoteForwardSpec("3000/sctp"); err == nil {
		t.Fatal("ParseRemoteForwardSpec accepted an unknown protocol suffix")
	}
}

// A hostname cannot contain '/', so the suffix cannot be confused with any part
// of the address grammar. This pins that the split happens at the LAST slash of
// the whole spec and nowhere inside a host.
func TestForwardSpecSuffixDoesNotEatTheHost(t *testing.T) {
	got, err := ParseForwardSpec("3000:db.internal:5432")
	if err != nil {
		t.Fatalf("ParseForwardSpec: %v", err)
	}
	if got.RemoteHost != "db.internal" {
		t.Errorf("RemoteHost = %q, want db.internal", got.RemoteHost)
	}
}
