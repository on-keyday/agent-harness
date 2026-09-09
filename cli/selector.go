package cli

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Runner selection, shared by every build. There used to be a whole second copy
// of this file under //go:build js, and only three lines of it actually differed
// per platform: one ParseConnectionID option, and whether --ip worked at all.
// Duplicating four functions for that is what let the wasm copy keep compiling
// against a type the native one had already moved off — caught only by
// `make check`'s GOOS=js build, since `go build ./...` never looks at it.
//
// What genuinely depends on the platform is now addrParseOptions() alone; see
// selector_addr_native.go / selector_addr_js.go.

// SelectorOpts holds the mutually-exclusive runner-selector options an operator
// can supply, from a CLI flag, the TUI command line or the WebUI command input.
// At most one field may be non-empty; ValidateSelector enforces that before
// buildSelector is called.
type SelectorOpts struct {
	// Runner pins a specific runner. Two forms, and they do not mean the same
	// thing — see buildRunnerIDSelector. The WebUI runner picker passes the
	// second form verbatim from RunnerCandidate.Cid.
	Runner string
	// Host is a plain hostname string, matched against what the runner reported
	// about itself in RunnerHello.
	Host string
	// IP is a dotted-decimal or colon-separated IP address string, matched
	// against the address the SERVER observed the runner connect from. So it and
	// Host can disagree, and Host is the one the runner gets to claim.
	IP string
}

// ValidateSelector returns an error if more than one field is set.
//
// Named with flags in the message even though the WebUI has no flags on its
// forms: the browser surface that can actually produce this error is the command
// input, where the operator typed `--ip` themselves.
func (s SelectorOpts) ValidateSelector() error {
	set := 0
	if s.Runner != "" {
		set++
	}
	if s.Host != "" {
		set++
	}
	if s.IP != "" {
		set++
	}
	if set > 1 {
		return fmt.Errorf("--runner, --host, and --ip are mutually exclusive; supply at most one")
	}
	return nil
}

// BuildSelector converts SelectorOpts into a protocol.RunnerSelector.
// It returns an error when the options are invalid or cannot be parsed.
// If all fields are empty, it returns RunnerSelectorKind_Any.
// This is the exported form; buildSelector is an alias used within the package.
func BuildSelector(opts SelectorOpts) (protocol.RunnerSelector, error) {
	return buildSelector(opts)
}

// buildSelector is the package-internal implementation used by tests.
//
// The arm order is not load-bearing: a caller is expected to have run
// ValidateSelector, so at most one field is set by the time this runs.
func buildSelector(opts SelectorOpts) (protocol.RunnerSelector, error) {
	switch {
	case opts.Runner != "":
		return buildRunnerIDSelector(opts.Runner)
	case opts.Host != "":
		var sel protocol.RunnerSelector
		sel.Kind = protocol.RunnerSelectorKind_ByHostname
		var h protocol.Hostname
		if !h.SetName([]byte(opts.Host)) {
			return protocol.RunnerSelector{}, fmt.Errorf("hostname too long: %q", opts.Host)
		}
		sel.SetHostname(h)
		return sel, nil
	case opts.IP != "":
		return buildIPSelector(opts.IP)
	default:
		return protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil
	}
}

// buildRunnerIDSelector parses what an operator puts after --runner into a
// selector. Two forms, and they no longer mean the same thing:
//
//   - 32 hex characters: a runner IDENTITY, the id= column of `harness-cli ls`.
//     Pins the runner PROCESS, so it still resolves after that runner reconnects.
//   - "ws:127.0.0.1:8539-123": a CONNECTION, the addr= column. Pins whatever
//     answers at that exact address, which a reconnect changes.
//
// The address form used to BE a RunnerID, so it went through the same arm; it
// needs its own now, and an operator who pastes the wrong column gets the
// weaker guarantee rather than an error. Hex is tried first: the address form
// has colons, so the two can never be confused.
func buildRunnerIDSelector(s string) (protocol.RunnerSelector, error) {
	var sel protocol.RunnerSelector
	if rid, err := protocol.RunnerIDFromHex(s); err == nil {
		sel.Kind = protocol.RunnerSelectorKind_ByRunnerId
		sel.SetRunnerId(rid)
		return sel, nil
	}
	cid, cidErr := objproto.ParseConnectionID(s, addrParseOptions())
	if cidErr != nil {
		return protocol.RunnerSelector{}, fmt.Errorf(
			"--runner: %q is neither a 32-hex runner id nor a connection id (%v); copy the id= or addr= value from `harness-cli ls`", s, cidErr)
	}
	sel.Kind = protocol.RunnerSelectorKind_ByConnId
	sel.SetConnId(protocol.ConnIDFromObjproto(cid))
	return sel, nil
}

// buildIPSelector parses an IP address string into a ByIp selector.
// Both IPv4 and IPv6 are accepted; IPv4-mapped IPv6 addresses are stored as
// 4-byte IPv4.
//
// Shared with the wasm build, which used to refuse --ip outright. That refusal
// was gratuitous: netip.ParseAddr and net.ParseIP are pure string parsers, so
// nothing here needs the OS network stack. The stated reason was that the
// browser has no IP field — true of the forms, and false of the command input,
// which reads --ip from the same declaration every other surface does and
// forwarded it only to be rejected at the end.
func buildIPSelector(ipStr string) (protocol.RunnerSelector, error) {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		// Fall back to net.ParseIP for formats netip doesn't accept
		raw := net.ParseIP(ipStr)
		if raw == nil {
			return protocol.RunnerSelector{}, fmt.Errorf("--ip: cannot parse IP address %q", ipStr)
		}
		if v4 := raw.To4(); v4 != nil {
			ip = netip.AddrFrom4([4]byte(v4))
		} else {
			ip = netip.AddrFrom16([16]byte(raw.To16()))
		}
	}

	var addrBytes []byte
	if ip.Is4() || ip.Is4In6() {
		a4 := ip.Unmap().As4()
		addrBytes = a4[:]
	} else {
		a16 := ip.As16()
		addrBytes = a16[:]
	}

	var sel protocol.RunnerSelector
	sel.Kind = protocol.RunnerSelectorKind_ByIp
	var addr protocol.IPAddr
	if !addr.SetAddr(addrBytes) {
		return protocol.RunnerSelector{}, fmt.Errorf("--ip: address too long")
	}
	sel.SetIpAddr(addr)
	return sel, nil
}
