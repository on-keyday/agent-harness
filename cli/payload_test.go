package cli

import (
	"strings"
	"testing"
)

// 2026-08-09: a reply body was shell-evaluated, `env` ran, and the whole
// environment -- this task's ticket included -- went to the board. Every
// publish reads its body through ResolvePayload, so the tripwire lives there
// and no publish verb can be the one that forgot it.
func TestResolvePayloadRefusesOwnTicket(t *testing.T) {
	const ticket = "0123456789abcdef0123456789abcdef"
	t.Setenv("HARNESS_AUTH_TICKET", ticket)
	if _, _, err := ResolvePayload(true, "env dump: HARNESS_AUTH_TICKET="+strings.ToUpper(ticket), "", nil); err == nil {
		t.Fatal("a body carrying this task's ticket was accepted")
	}
	if _, _, err := ResolvePayload(true, "ordinary body", "", nil); err != nil {
		t.Fatalf("ordinary body: %v", err)
	}
}
