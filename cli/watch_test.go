package cli_test

import "testing"

// TestWatch_E2E is deferred to integration tests.
//
// Rationale: Watch requires a live server with pubsub and at least one task
// lifecycle event to verify end-to-end. Unit-testing it in isolation would
// require mocking the full trsf/pubsub stack, which adds more complexity than
// value at this stage. Integration coverage lives in integration/.
func TestWatch_E2E(t *testing.T) {
	t.Skip("deferred to E2E integration tests — requires live server")
}
