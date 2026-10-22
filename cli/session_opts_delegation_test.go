package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Both wire builders carry the delegation bound and its two presence bits;
// a field set in one builder and missed in the other is the failure mode
// SessionOpts exists to end.
func TestSessionOptsCarryBound(t *testing.T) {
	scope := protocol.TaskScope{Base: protocol.ScopeBase_None}
	scope.SetExcludeSelf(true)
	opts := SessionOpts{
		DelegateCaps: protocol.Capability_FileRead, DelegateScope: scope,
		DelegateCapsPresent: true, DelegateScopePresent: true,
	}
	sub := buildSubmitRequest("/r", "p", opts)
	if sub.DelegateCaps != protocol.Capability_FileRead || sub.DelegateScope.Base != protocol.ScopeBase_None ||
		!sub.DelegateScope.ExcludeSelf() || !sub.DelegateCapsPresent() || !sub.DelegateScopePresent() {
		t.Errorf("buildSubmitRequest lost the bound: %+v", sub)
	}
	oi := buildOpenInteractiveRequest("/r", opts)
	if oi.DelegateCaps != protocol.Capability_FileRead || oi.DelegateScope.Base != protocol.ScopeBase_None ||
		!oi.DelegateScope.ExcludeSelf() || !oi.DelegateCapsPresent() || !oi.DelegateScopePresent() {
		t.Errorf("buildOpenInteractiveRequest lost the bound: %+v", oi)
	}
}

// session new is how agents spawn workers, so the server's refusal text —
// which bound refused which id — must reach them, not a fixed sentence.
func TestOpenInteractiveRefusalCarriesServerText(t *testing.T) {
	msg := "scope id ab is outside what you may delegate (see whoami: delegate-scope)"
	err := openInteractiveStatusError("/r", "", protocol.OpenInteractiveStatus_ScopeNotPermitted, []byte(msg))
	if err == nil || !strings.Contains(err.Error(), msg) {
		t.Errorf("err = %v, want it to carry %q", err, msg)
	}
	// No text from the server: the fixed sentence, as before.
	if err := openInteractiveStatusError("/r", "", protocol.OpenInteractiveStatus_ScopeNotPermitted, nil); err == nil || !strings.Contains(err.Error(), "outside your own scope") {
		t.Errorf("err = %v, want the fixed text", err)
	}
}
