//go:build !js

package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestSubmitSelectorOpts verifies mutual-exclusion validation.
func TestSubmitSelectorOpts(t *testing.T) {
	tests := []struct {
		name    string
		opts    SelectorOpts
		wantErr bool
	}{
		{name: "empty is ok", opts: SelectorOpts{}, wantErr: false},
		{name: "host only ok", opts: SelectorOpts{Host: "myhost"}, wantErr: false},
		{name: "ip only ok", opts: SelectorOpts{IP: "1.2.3.4"}, wantErr: false},
		{name: "runner only ok", opts: SelectorOpts{Runner: "deadbeef"}, wantErr: false},
		{name: "host+ip conflict", opts: SelectorOpts{Host: "x", IP: "1.2.3.4"}, wantErr: true},
		{name: "host+runner conflict", opts: SelectorOpts{Host: "x", Runner: "aa"}, wantErr: true},
		{name: "ip+runner conflict", opts: SelectorOpts{IP: "1.2.3.4", Runner: "aa"}, wantErr: true},
		{name: "all three conflict", opts: SelectorOpts{Host: "x", IP: "1.2.3.4", Runner: "aa"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.ValidateSelector()
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateSelector() error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestBuildSelectorAny verifies that empty opts produce Kind_Any.
func TestBuildSelectorAny(t *testing.T) {
	sel, err := buildSelector(SelectorOpts{})
	if err != nil {
		t.Fatalf("buildSelector empty: %v", err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_Any {
		t.Errorf("expected Any, got %v", sel.Kind)
	}
}

// TestBuildSelectorByHostname verifies hostname selector construction.
func TestBuildSelectorByHostname(t *testing.T) {
	sel, err := buildSelector(SelectorOpts{Host: "gmkhost"})
	if err != nil {
		t.Fatalf("buildSelector host: %v", err)
	}
	if sel.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Errorf("expected ByHostname, got %v", sel.Kind)
	}
	h := sel.Hostname()
	if h == nil || string(h.Name) != "gmkhost" {
		t.Errorf("hostname mismatch: %v", h)
	}
}

// TestBuildSelectorByIP verifies IPv4 and IPv6 selector construction.
func TestBuildSelectorByIP(t *testing.T) {
	tests := []struct {
		ip      string
		addrLen int
	}{
		{"192.168.1.1", 4},
		{"::1", 16},
	}
	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			sel, err := buildSelector(SelectorOpts{IP: tc.ip})
			if err != nil {
				t.Fatalf("buildSelector ip=%s: %v", tc.ip, err)
			}
			if sel.Kind != protocol.RunnerSelectorKind_ByIp {
				t.Errorf("expected ByIp, got %v", sel.Kind)
			}
			addr := sel.IpAddr()
			if addr == nil || len(addr.Addr) != tc.addrLen {
				t.Errorf("addr mismatch: %v", addr)
			}
		})
	}
}

// TestBuildSelectorByIPInvalid verifies that a bad IP returns an error.
func TestBuildSelectorByIPInvalid(t *testing.T) {
	_, err := buildSelector(SelectorOpts{IP: "not-an-ip"})
	if err == nil {
		t.Error("expected error for invalid IP, got nil")
	}
}

// TestBuildSelectorByRunnerInvalidHex verifies that bad hex returns an error.
func TestBuildSelectorByRunnerInvalidHex(t *testing.T) {
	_, err := buildSelector(SelectorOpts{Runner: "ZZZZ"})
	if err == nil {
		t.Error("expected error for invalid hex, got nil")
	}
}

// TestSubmitStatusError verifies that non-Ok statuses map to named errors.
func TestSubmitStatusError(t *testing.T) {
	tests := []struct {
		status  protocol.SubmitStatus
		errMsg  string
		wantSub string
	}{
		{protocol.SubmitStatus_Ok, "", ""},
		{protocol.SubmitStatus_NoRunner, "", "no_runner"},
		{protocol.SubmitStatus_AmbiguousRunner, "matches: gmkhost, raspi", "ambiguous_runner"},
		{protocol.SubmitStatus_PinnedNotFound, "", "pinned_not_found"},
		{protocol.SubmitStatus_InternalError, "", "internal_error"},
	}
	for _, tc := range tests {
		t.Run(tc.status.String(), func(t *testing.T) {
			sr := &protocol.SubmitResponse{Status: tc.status}
			if tc.errMsg != "" {
				sr.SetErrorMsg([]byte(tc.errMsg))
			}
			err := submitStatusError(sr)
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
			// AmbiguousRunner should include the server-provided match list
			if tc.status == protocol.SubmitStatus_AmbiguousRunner && tc.errMsg != "" {
				if !contains(err.Error(), tc.errMsg) {
					t.Errorf("error %q does not include match list %q", err.Error(), tc.errMsg)
				}
			}
		})
	}
}

// TestBuildSubmitRequestAgentProfile verifies that a non-empty agentProfile
// passed to buildSubmitRequest (and therefore SubmitWithSelectorArgsAndCaps)
// ends up set on the wire SubmitRequest.
func TestBuildSubmitRequestAgentProfile(t *testing.T) {
	sub := buildSubmitRequest("/repo", "prompt", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, protocol.Capability_All, false, false, "codex")
	if string(sub.AgentProfile) != "codex" {
		t.Errorf("AgentProfile = %q, want %q", sub.AgentProfile, "codex")
	}
}

// TestBuildSubmitRequestAgentProfileEmpty verifies the default ("") case
// leaves AgentProfile empty, so existing callers that don't pass a profile
// are unaffected.
func TestBuildSubmitRequestAgentProfileEmpty(t *testing.T) {
	sub := buildSubmitRequest("/repo", "prompt", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, protocol.Capability_All, false, false, "")
	if len(sub.AgentProfile) != 0 {
		t.Errorf("AgentProfile = %q, want empty", sub.AgentProfile)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexStr(s, sub) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
