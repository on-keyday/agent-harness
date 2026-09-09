package cliopts

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// ResolveServerCID returns the ConnectionID from the flag value or HARNESS_SERVER_CID env.
// Flag wins over env. Returns error if both are empty.
func ResolveServerCID(flagVal string) (objproto.ConnectionID, error) {
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("HARNESS_SERVER_CID")
	}
	if raw == "" {
		return objproto.ConnectionID{}, errors.New("--server-cid required (or set HARNESS_SERVER_CID)")
	}
	return objproto.ParseConnectionID(raw, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
}

// ResolveAuthTicket reads HARNESS_AUTH_TICKET only (no flag fallback).
// Returns error if env is unset or not 32 hex chars (16 bytes).
func ResolveAuthTicket() ([16]byte, error) {
	var t [16]byte
	raw := os.Getenv("HARNESS_AUTH_TICKET")
	if raw == "" {
		return t, errors.New("HARNESS_AUTH_TICKET env required (no flag accepted)")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return t, fmt.Errorf("HARNESS_AUTH_TICKET: %w", err)
	}
	if len(b) != 16 {
		return t, fmt.Errorf("HARNESS_AUTH_TICKET: expected 16 bytes, got %d", len(b))
	}
	copy(t[:], b)
	return t, nil
}

// ResolveTaskID reads from flag or HARNESS_TASK_ID env (32 hex chars = 16 bytes).
func ResolveTaskID(flagVal string) (protocol.TaskID, error) {
	var t protocol.TaskID
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("HARNESS_TASK_ID")
	}
	if raw == "" {
		return t, errors.New("--task-id required (or set HARNESS_TASK_ID)")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return t, fmt.Errorf("task-id: %w", err)
	}
	if len(b) != 16 {
		return t, fmt.Errorf("task-id: expected 16 bytes, got %d", len(b))
	}
	copy(t.Id[:], b)
	return t, nil
}

// ResolveRunnerID parses the runner IDENTITY from flag or HARNESS_RUNNER_ID:
// 32 hex characters naming a runner process. It used to parse a ConnectionID,
// because the identity WAS one — the runner had to convert its canonical id to
// an address just to put it in the env var, and this end parsed it back. Both
// halves of that laundering are gone.
func ResolveRunnerID(flagVal string) (protocol.RunnerID, error) {
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("HARNESS_RUNNER_ID")
	}
	if raw == "" {
		return protocol.RunnerID{}, errors.New("--runner-id required (or set HARNESS_RUNNER_ID)")
	}
	rid, err := protocol.RunnerIDFromHex(raw)
	if err != nil {
		return protocol.RunnerID{}, fmt.Errorf("runner-id: %w", err)
	}
	return rid, nil
}

// ResolveString returns flagVal if non-empty, otherwise the value of envName.
func ResolveString(flagVal, envName string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envName)
}

// ResolveStringWith is ResolveString with a third tier: a value from the
// workspace config, consulted only when neither the flag nor the environment
// supplied one.
//
// It takes the VALUE rather than the config file so this package does not
// import cli/workspace: the resolution order lives here, the file format lives
// there.
func ResolveStringWith(flagVal, envName, fileVal string) string {
	if v := ResolveString(flagVal, envName); v != "" {
		return v
	}
	return fileVal
}
