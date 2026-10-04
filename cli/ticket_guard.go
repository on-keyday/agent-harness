package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
)

// RefuseIfOwnTicket blocks a publish whose payload contains this task's own
// HARNESS_AUTH_TICKET. 2026-08-09: a reply body was shell-evaluated, `env`
// ran, and the whole environment went to the board. ResolvePayload calls it,
// so every publish verb that reads a body through there is covered.
//
// This is a tripwire for one known string, not a secret scanner — nothing
// else in the payload is inspected.
func RefuseIfOwnTicket(payload []byte) error {
	ticket := os.Getenv("HARNESS_AUTH_TICKET")
	if len(ticket) < 16 {
		return nil
	}
	if bytes.Contains(bytes.ToLower(payload), []byte(strings.ToLower(ticket))) {
		return errors.New("refusing to publish: payload contains this task's HARNESS_AUTH_TICKET (was the body shell-evaluated?)")
	}
	return nil
}
