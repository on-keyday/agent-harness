package cli

import (
	"fmt"
	"net"
	"strings"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

// A candidate is one address in a list a process keeps dialing: a dial-mode
// runner's --server-cid, and a server's dial record. Both keep the TEXT and
// resolve it on every attempt, so a host name follows an address that moves.
// The runner and the server share these functions so the two lists cannot
// disagree about what a candidate is.

// SplitCandidates splits a comma-separated list into its ordered entries
// without resolving them. An empty entry is an error rather than something to
// skip: a stray or trailing comma is a typo, and dropping it silently would
// dial a list nobody wrote.
func SplitCandidates(spec string) ([]string, error) {
	parts := strings.Split(spec, ",")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("candidate %d of %d is empty (stray or trailing comma in %q)", i+1, len(parts), spec)
		}
		out = append(out, p)
	}
	return out, nil
}

// ResolveCandidate turns one candidate into a dialable ConnectionID. DNS
// happens here, once per dial attempt, and `-*` draws a fresh id each time.
// The error names the candidate: with a list, objproto's own "invalid
// connection ID format" does not say which entry was mistyped.
func ResolveCandidate(spec string) (objproto.ConnectionID, error) {
	cid, err := objproto.ParseConnectionID(spec,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		return objproto.ConnectionID{}, fmt.Errorf("candidate %q: %w", spec, err)
	}
	return cid, nil
}

// ParseCandidate checks a candidate's grammar WITHOUT resolving it, so a host
// name that does not resolve yet is accepted where it will be dialed later. A
// literal address goes straight to objproto; a host name is checked by
// parsing the same text with the host replaced by a literal, which leaves
// every other part (transport, port, id) to objproto's own parser.
func ParseCandidate(spec string) error {
	if _, err := objproto.ParseConnectionID(spec, objproto.ParseOption_AllowRandomID); err == nil {
		return nil
	}
	bad := fmt.Errorf("candidate %q: want transport:host:port-id, e.g. ws:runner.example:8540-*", spec)
	// objproto's own first two cuts: transport at the first ':', id after the
	// last '-' (a host name may hold '-', an id never does).
	transport, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return bad
	}
	sep := strings.LastIndexByte(rest, '-')
	if sep < 0 {
		return bad
	}
	addr, id := rest[:sep], rest[sep+1:]
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return bad
	}
	if _, err := objproto.ParseConnectionID(transport+":"+net.JoinHostPort("0.0.0.0", port)+"-"+id,
		objproto.ParseOption_AllowRandomID); err != nil {
		return fmt.Errorf("candidate %q: %w", spec, err)
	}
	return nil
}

// CandidateTransport is the text before the first ':' — the one part of a
// candidate that needs no resolution.
func CandidateTransport(spec string) string {
	t, _, _ := strings.Cut(spec, ":")
	return t
}

// IsURLCandidate reports whether a candidate is a ws:// or wss:// URL rather
// than a transport:host:port-id. Only a dial record takes a URL: its address
// is a named one, which means something only to the server that registered
// it (objproto.NamedAddrPort), so it is never handed to anyone else to dial.
func IsURLCandidate(text string) bool { return strings.Contains(text, "://") }

// CanonicalDialRecordCandidate checks one dial-record candidate and returns
// the text to store: a URL in canonical form (transport.ParseWebSocketTarget,
// which names the rule a refused URL broke), anything else as given once
// ParseCandidate accepts it.
func CanonicalDialRecordCandidate(text string) (string, error) {
	if IsURLCandidate(text) {
		t, err := transport.ParseWebSocketTarget(text)
		if err != nil {
			return "", err
		}
		return t.String(), nil
	}
	if err := ParseCandidate(text); err != nil {
		return "", err
	}
	return text, nil
}
