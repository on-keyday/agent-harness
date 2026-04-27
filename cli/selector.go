//go:build !js

package cli

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SelectorOpts holds the mutually-exclusive runner-selector flags that the
// CLI user can supply. At most one field may be non-empty; ValidateSelector
// enforces this constraint before buildSelector is called.
type SelectorOpts struct {
	// Runner is a 32-char hex ConnectionID string (RunnerID pin).
	Runner string
	// Host is a plain hostname string.
	Host string
	// IP is a dotted-decimal or colon-separated IP address string.
	IP string
}

// ValidateSelector returns an error if more than one field is set.
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

// buildSelector converts SelectorOpts into a protocol.RunnerSelector.
// It returns an error when the options are invalid or cannot be parsed.
// If all fields are empty, it returns RunnerSelectorKind_Any.
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

// buildRunnerIDSelector parses a hex ConnectionID string into a ByRunnerId selector.
// The hex string must encode a RunnerID whose IpAddr is 4 or 16 bytes; a zero
// IpAddr (IpAddrLen=0) is rejected because the protocol encoder panics on it.
func buildRunnerIDSelector(hexStr string) (protocol.RunnerSelector, error) {
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return protocol.RunnerSelector{}, fmt.Errorf("--runner: invalid hex: %w", err)
	}
	var rid protocol.RunnerID
	if _, err := rid.Decode(raw); err != nil {
		return protocol.RunnerSelector{}, fmt.Errorf("--runner: cannot decode RunnerID: %w", err)
	}
	// IpAddrLen must be 4 or 16; zero causes encoder panic.
	if rid.IpAddrLen == 0 {
		return protocol.RunnerSelector{}, fmt.Errorf("--runner: RunnerID has no IP address; use --host or --ip instead")
	}
	var sel protocol.RunnerSelector
	sel.Kind = protocol.RunnerSelectorKind_ByRunnerId
	sel.SetRunnerId(rid)
	return sel, nil
}

// buildIPSelector parses an IP address string into a ByIp selector.
// Both IPv4 and IPv6 are accepted; IPv4-mapped IPv6 addresses are stored as
// 4-byte IPv4.
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
