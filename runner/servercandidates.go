package runner

import (
	"fmt"
	"strings"

	"github.com/on-keyday/objtrsf/objproto"
)

// ServerCandidates is the ordered list of server addresses a dial-mode runner
// tries, one after another, until one answers. `--server-cid` takes it as a
// comma-separated list; a runner configured with a single address is a list of
// one, not a separate path.
//
// It holds TEXT rather than parsed ConnectionIDs, and that is the whole point.
// Parsing resolves the host — objproto.ParseConnectionID with
// ParseOption_ResolveAddr calls net.LookupIP — so a name that only resolves on
// the home LAN fails at PARSE time once the laptop is somewhere else, not at
// dial time. Resolving the list once at startup would therefore discard the
// address the runner is about to need, and a runner an autostart unit starts at
// login, before the network is up, would resolve nothing at all. Each dial
// attempt re-reads the text instead.
//
// Only the runner takes a list. harness-cli, the TUI and the WebUI take one
// address: an operator moving between networks knows which one they are on and
// switches by hand, and the runner is the process that has to cross that change
// with nobody present. The agents a runner spawns take one too — the runner
// injects HARNESS_SERVER_CID as the address it actually connected on, never
// this list.
type ServerCandidates struct {
	specs []string
}

// ParseServerCandidates splits a --server-cid value into its ordered entries.
// It does NOT resolve them; ResolveServerCandidate does, per dial attempt.
//
// An empty entry is an error rather than something to skip: a stray or trailing
// comma is a typo, and dropping it silently would dial a list the operator did
// not write. Nothing else about a candidate is checked here — the grammar of a
// ConnectionID belongs to objproto, and restating it would be a second copy to
// keep in step.
func ParseServerCandidates(spec string) (ServerCandidates, error) {
	parts := strings.Split(spec, ",")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return ServerCandidates{}, fmt.Errorf(
				"--server-cid: candidate %d of %d is empty (stray or trailing comma in %q)",
				i+1, len(parts), spec)
		}
		out = append(out, p)
	}
	return ServerCandidates{specs: out}, nil
}

// CandidatesOf builds a list from addresses that are already resolved — the
// integration suite, and any embedder that holds a ConnectionID rather than the
// operator's text. It goes through String() so the type has exactly one
// representation; that round trip is the one every agent already makes, since
// the runner injects HARNESS_SERVER_CID as String() and harness-cli parses it
// back.
func CandidatesOf(cids ...objproto.ConnectionID) ServerCandidates {
	out := make([]string, 0, len(cids))
	for _, c := range cids {
		out = append(out, c.String())
	}
	return ServerCandidates{specs: out}
}

// Len reports how many candidates there are. Zero means listen mode, where the
// server dials the runner and there is no address to try.
func (c ServerCandidates) Len() int { return len(c.specs) }

// All returns the candidates in the order they will be tried.
func (c ServerCandidates) All() []string { return c.specs }

// String renders the list back in the spelling --server-cid accepts.
func (c ServerCandidates) String() string { return strings.Join(c.specs, ",") }

// Schemes returns the distinct transport prefixes in the list, in order of
// first appearance.
//
// The transport is the one part of a ConnectionID that needs NO resolution — it
// is the text before the first ':', which is how objproto itself starts parsing
// — so the endpoint's shape can be decided from the whole list up front while
// each address is still resolved per attempt. That ordering matters: a list
// spanning ws and udp needs one endpoint carrying BOTH legs, and which legs are
// needed cannot wait for a DNS lookup that may be failing right now.
//
// Unrecognised transports are returned as they were written. Deciding what is
// dialable is endpointLegsFor's job, and a candidate whose transport nothing
// can dial fails when it is dialed rather than sinking the list.
func (c ServerCandidates) Schemes() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range c.specs {
		scheme, _, _ := strings.Cut(s, ":")
		if seen[scheme] {
			continue
		}
		seen[scheme] = true
		out = append(out, scheme)
	}
	return out
}

// ResolveServerCandidate turns one candidate into a dialable ConnectionID. DNS
// happens here, once per dial attempt.
//
// The error names the candidate. With a list, objproto's own "invalid
// connection ID format" does not say which entry was mistyped.
func ResolveServerCandidate(spec string) (objproto.ConnectionID, error) {
	cid, err := objproto.ParseConnectionID(spec,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		return objproto.ConnectionID{}, fmt.Errorf("server candidate %q: %w", spec, err)
	}
	return cid, nil
}
