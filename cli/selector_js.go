//go:build js

package cli

import (
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// SelectorOpts holds the mutually-exclusive runner-selector flags that the
// CLI user can supply. At most one field may be non-empty; ValidateSelector
// enforces this constraint before buildSelector is called.
//
// This is the js/wasm variant. IP is not exposed in the browser UI (no raw
// IP-string entry point), so it produces an error at runtime. Runner IS
// supported: it is the ConnectionID string round-tripped verbatim from
// cli.RunnerCandidate.Cid (see interactive_errors.go) so the WebUI runner
// picker can pin a retry — parsing it needs no OS-level networking (it is
// already a literal ip:port), so it works fine under GOOS=js.
type SelectorOpts struct {
	// Runner is a ConnectionID string (e.g. "ws:127.0.0.1:8539-123"), as
	// returned verbatim by RunnerCandidate.Cid; a hex-encoded RunnerID is
	// also accepted for back-compat with the native --runner flag format.
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
		return fmt.Errorf("runner, host, and ip are mutually exclusive; supply at most one")
	}
	return nil
}

// BuildSelector converts SelectorOpts into a protocol.RunnerSelector.
// In the wasm build, Host (ByHostname), Runner (ByRunnerId, pinned via a
// RunnerCandidate.Cid string), and the default Any selector are supported.
// IP requires raw address-string entry not exposed in the browser UI.
func BuildSelector(opts SelectorOpts) (protocol.RunnerSelector, error) {
	return buildSelector(opts)
}

func buildSelector(opts SelectorOpts) (protocol.RunnerSelector, error) {
	switch {
	case opts.Runner != "":
		return buildRunnerIDSelector(opts.Runner)
	case opts.IP != "":
		return protocol.RunnerSelector{}, fmt.Errorf("IP selector not supported in wasm build; use host")
	case opts.Host != "":
		var sel protocol.RunnerSelector
		sel.Kind = protocol.RunnerSelectorKind_ByHostname
		var h protocol.Hostname
		if !h.SetName([]byte(opts.Host)) {
			return protocol.RunnerSelector{}, fmt.Errorf("hostname too long: %q", opts.Host)
		}
		sel.SetHostname(h)
		return sel, nil
	default:
		return protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil
	}
}

// buildRunnerIDSelector parses a runner identifier into a ByRunnerId selector.
// This is the wasm counterpart of the native selector.go's function of the
// same name (kept intentionally close so both build variants accept the same
// two input shapes). It accepts the ConnectionID string form that the WebUI
// picker round-trips from RunnerCandidate.Cid (e.g. "ws:127.0.0.1:8539-123")
// and, for back-compat, a hex-encoded RunnerID.
//
// Unlike the native variant, this never passes objproto.ParseOption_ResolveAddr:
// that option falls back to net.LookupIP for hostname-shaped addr strings,
// and DNS resolution is not available under GOOS=js. This is not a
// functional gap here — the WebUI never types a hostname into this field,
// it only ever passes back a literal cid string the server already gave it.
func buildRunnerIDSelector(s string) (protocol.RunnerSelector, error) {
	var rid protocol.RunnerID
	if cid, cidErr := objproto.ParseConnectionID(s, objproto.ParseOption_AllowRandomID); cidErr == nil {
		rid = protocol.ConnIDToRunnerID(cid)
	} else if raw, hexErr := hex.DecodeString(s); hexErr == nil {
		if _, derr := rid.Decode(raw); derr != nil {
			return protocol.RunnerSelector{}, fmt.Errorf("runner: cannot decode RunnerID hex: %w", derr)
		}
	} else {
		return protocol.RunnerSelector{}, fmt.Errorf("runner: %q is neither a ConnectionID (%v) nor hex (%v)", s, cidErr, hexErr)
	}
	// IpAddrLen must be 4 or 16; zero causes encoder panic (project_runnerid_constraint).
	if rid.IpAddrLen == 0 {
		return protocol.RunnerSelector{}, fmt.Errorf("runner: RunnerID has no IP address; use host instead")
	}
	var sel protocol.RunnerSelector
	sel.Kind = protocol.RunnerSelectorKind_ByRunnerId
	sel.SetRunnerId(rid)
	return sel, nil
}
