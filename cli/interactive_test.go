//go:build !js

package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestInteractiveSelectorShared verifies that SelectorOpts / ValidateSelector /
// buildSelector are usable from the interactive code path (shared helpers).
func TestInteractiveSelectorShared(t *testing.T) {
	// Verify the same ValidateSelector logic applies to interactive callers.
	opts := SelectorOpts{Host: "raspi", IP: "10.0.0.1"}
	if err := opts.ValidateSelector(); err == nil {
		t.Error("expected mutual-exclusion error, got nil")
	}
}

// TestInteractiveBuildSelectorAny verifies that empty opts yield Any selector.
func TestInteractiveBuildSelectorAny(t *testing.T) {
	sel, err := buildSelector(SelectorOpts{})
	if err != nil {
		t.Fatalf("buildSelector: %v", err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_Any {
		t.Errorf("expected Any, got %v", sel.Kind)
	}
}

// TestInteractiveBuildSelectorByHost verifies hostname selector for interactive.
func TestInteractiveBuildSelectorByHost(t *testing.T) {
	sel, err := buildSelector(SelectorOpts{Host: "raspi"})
	if err != nil {
		t.Fatalf("buildSelector: %v", err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Errorf("expected ByHostname, got %v", sel.Kind)
	}
	h := sel.Hostname()
	if h == nil || string(h.Name) != "raspi" {
		t.Errorf("hostname mismatch: %v", h)
	}
}

// TestOpenInteractiveStatusError covers all OpenInteractiveStatus branches.
func TestOpenInteractiveStatusError(t *testing.T) {
	tests := []struct {
		status  protocol.OpenInteractiveStatus
		wantSub string // empty string = expect nil error
	}{
		{protocol.OpenInteractiveStatus_Ok, ""},
		{protocol.OpenInteractiveStatus_NoRunnerForRepo, "no_runner_for_repo"},
		{protocol.OpenInteractiveStatus_RunnerBusy, "runner_busy"},
		{protocol.OpenInteractiveStatus_AmbiguousRunner, "ambiguous_runner"},
		{protocol.OpenInteractiveStatus_PinnedNotFound, "pinned_not_found"},
		{protocol.OpenInteractiveStatus_ProfileUnavailable, "profile_unavailable"},
		{protocol.OpenInteractiveStatus_InternalError, "internal_error"},
	}
	for _, tc := range tests {
		t.Run(tc.status.String(), func(t *testing.T) {
			err := openInteractiveStatusError("/repo/path", "codex", tc.status)
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestBuildOpenInteractiveRequestAgentProfile verifies that a non-empty
// agentProfile passed to buildOpenInteractiveRequest (and therefore
// OpenInteractiveWithSelectorArgsAndCaps / openInteractive) ends up set on
// the wire OpenInteractiveRequest.
func TestBuildOpenInteractiveRequestAgentProfile(t *testing.T) {
	oi := buildOpenInteractiveRequest("/repo", SessionOpts{Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, Caps: CapsPtr(protocol.Capability_All), AgentProfile: "codex"})
	if string(oi.AgentProfile) != "codex" {
		t.Errorf("AgentProfile = %q, want %q", oi.AgentProfile, "codex")
	}
}

// TestBuildOpenInteractiveRequestAgentProfileEmpty verifies the default ("")
// case leaves AgentProfile empty, so existing callers that don't pass a
// profile are unaffected.
func TestBuildOpenInteractiveRequestAgentProfileEmpty(t *testing.T) {
	oi := buildOpenInteractiveRequest("/repo", SessionOpts{Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, Caps: CapsPtr(protocol.Capability_All)})
	if len(oi.AgentProfile) != 0 {
		t.Errorf("AgentProfile = %q, want empty", oi.AgentProfile)
	}
}

// TestInteractiveE2E is deferred to integration tests (requires live server + runner PTY).
func TestInteractiveE2E(t *testing.T) {
	t.Skip("deferred to E2E integration tests — requires live server and runner with PTY")
}
