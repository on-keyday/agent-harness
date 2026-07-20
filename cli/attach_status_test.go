package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// IsAttachPermanent must classify only "the session is gone/finished" statuses
// as permanent. Everything else (runner momentarily unreachable, internal
// error, an observer dropped for falling behind => surfaced as a plain stream
// error, not a status) is transient, so the grid's pump reattaches instead of
// going permanently black.
func TestIsAttachPermanent(t *testing.T) {
	permanent := []protocol.AttachSessionStatus{
		protocol.AttachSessionStatus_NotFound,
		protocol.AttachSessionStatus_AlreadyTerminal,
	}
	for _, s := range permanent {
		err := attachStatusError("deadbeef", s)
		if err == nil {
			t.Fatalf("status %v must be an error", s)
		}
		if !IsAttachPermanent(err) {
			t.Fatalf("status %v must be permanent (no retry), got %v", s, err)
		}
	}

	transient := []protocol.AttachSessionStatus{
		protocol.AttachSessionStatus_RunnerUnreachable,
		protocol.AttachSessionStatus_InternalError,
		protocol.AttachSessionStatus_NotInteractive,
	}
	for _, s := range transient {
		err := attachStatusError("deadbeef", s)
		if err == nil {
			t.Fatalf("status %v must be an error", s)
		}
		if IsAttachPermanent(err) {
			t.Fatalf("status %v must be transient (reattach), got %v", s, err)
		}
	}

	// A bare non-attach error (e.g. a dropped-observer stream EOF) is transient.
	if IsAttachPermanent(errors.New("EOF")) {
		t.Fatal("an unrelated error must not be treated as permanent")
	}
	// Wrapping must preserve the classification (pump wraps/annotates errors).
	wrapped := fmt.Errorf("pump: %w", attachStatusError("x", protocol.AttachSessionStatus_AlreadyTerminal))
	if !IsAttachPermanent(wrapped) {
		t.Fatal("wrapped terminal error must remain permanent via errors.Is")
	}
}
