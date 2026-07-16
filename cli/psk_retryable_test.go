package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The fatal/retryable split must be drawn at "is this a credential failure?",
// not at "did the server reject us?". NoIdentity means the server could not
// decode the identity union we DID send — in practice a wire/schema skew that
// fixes itself once the server is upgraded. Classifying it fatal is what wiped
// the whole runner fleet on 2026-07-16 (every slot exited on the first rejection
// and none came back), so this table is a real regression guard.
func TestPskRejectedErrorRetryable(t *testing.T) {
	tests := []struct {
		code          protocol.PskAuthStatus
		wantRetryable bool
	}{
		{protocol.PskAuthStatus_NoIdentity, true}, // wire/version skew — retry once the server is upgraded
		{protocol.PskAuthStatus_BadPsk, false},    // wrong PSK — no retry fixes it
		{protocol.PskAuthStatus_BadTicket, false}, // invalid agent ticket — no retry fixes it
	}
	for _, tc := range tests {
		t.Run(tc.code.String(), func(t *testing.T) {
			e := &PskRejectedError{Status: tc.code.String(), Code: tc.code}
			if got := e.Retryable(); got != tc.wantRetryable {
				t.Errorf("Retryable() = %v, want %v for %v", got, tc.wantRetryable, tc.code)
			}
		})
	}
}
