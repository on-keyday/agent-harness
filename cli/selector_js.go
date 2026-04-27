//go:build js

package cli

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SelectorOpts holds the mutually-exclusive runner-selector flags that the
// CLI user can supply. At most one field may be non-empty; ValidateSelector
// enforces this constraint before buildSelector is called.
//
// This is the js/wasm variant. IP and Runner selectors are not exposed in the
// browser UI (no raw IP parsing available via net/netip), so only Host is
// supported. Runner and IP fields are accepted for API symmetry but produce
// an error at runtime.
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
		return fmt.Errorf("runner, host, and ip are mutually exclusive; supply at most one")
	}
	return nil
}

// BuildSelector converts SelectorOpts into a protocol.RunnerSelector.
// In the wasm build only Host (ByHostname) and the default Any selector are
// supported. Runner and IP selectors require native net parsing and are not
// available in the browser.
func BuildSelector(opts SelectorOpts) (protocol.RunnerSelector, error) {
	return buildSelector(opts)
}

func buildSelector(opts SelectorOpts) (protocol.RunnerSelector, error) {
	switch {
	case opts.Runner != "":
		return protocol.RunnerSelector{}, fmt.Errorf("runner selector not supported in wasm build; use host")
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
