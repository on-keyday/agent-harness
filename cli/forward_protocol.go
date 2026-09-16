package cli

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// splitForwardProtocol peels a trailing "/tcp" or "/udp" off a forward spec and
// returns the rest plus what it names. No suffix means tcp, so every spelling
// that existed before this axis still parses and still means what it did.
//
// The suffix rather than a flag: `-L` is repeatable, and protocol is a property
// of ONE forward -- a flag would force every spec in an invocation to agree.
// docker's `-p 5353:5353/udp` is the same shape with the same suffix, which is
// the closest widely-known precedent for a colon-separated port spec.
//
// It splits at the last '/' of the WHOLE spec, and that is unambiguous because
// a hostname cannot contain '/' and IPv6 literals are already unsupported by
// both parsers.
//
// An unrecognised suffix names what is accepted. Falling through to the colon
// grammar's "bad spec" would send the reader to the wrong half of the string.
func splitForwardProtocol(s string) (string, protocol.ForwardProtocol, error) {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return s, protocol.ForwardProtocol_Tcp, nil
	}
	rest, suffix := s[:i], s[i+1:]
	switch suffix {
	case "tcp":
		return rest, protocol.ForwardProtocol_Tcp, nil
	case "udp":
		return rest, protocol.ForwardProtocol_Udp, nil
	default:
		return "", 0, fmt.Errorf("forward: bad protocol suffix %q in %q (want /tcp or /udp, or no suffix for tcp)", suffix, s)
	}
}

// forwardDialNetwork is the net.Dial network a forward's protocol implies.
//
// Distinct from RemoteForwardSpec.DialNetwork, which the caller may override to
// "unix" for X11: that field says which SOCKET the client opens for its own
// target, while the protocol says what crosses the tunnel. They agree for tcp
// and udp and diverge only for the unix case, which is stream-oriented and so
// still a tcp forward on the wire.
func forwardDialNetwork(p protocol.ForwardProtocol) string {
	if p == protocol.ForwardProtocol_Udp {
		return "udp"
	}
	return "tcp"
}
