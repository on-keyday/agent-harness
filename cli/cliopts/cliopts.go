package cliopts

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
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

// ResolveRunnerID parses a runner ConnectionID from flag or HARNESS_RUNNER_ID
// and converts to protocol.RunnerID (transport+ip+port+unique).
func ResolveRunnerID(flagVal string) (protocol.RunnerID, error) {
	var rid protocol.RunnerID
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("HARNESS_RUNNER_ID")
	}
	if raw == "" {
		return rid, errors.New("--runner-id required (or set HARNESS_RUNNER_ID)")
	}
	cid, err := objproto.ParseConnectionID(raw, objproto.ParseOption_ResolveAddr)
	if err != nil {
		return rid, fmt.Errorf("runner-id: %w", err)
	}
	rid.SetTransport([]byte(cid.Transport))
	if cid.Addr.Addr().Is4() {
		ip4 := cid.Addr.Addr().As4()
		rid.SetIpAddr(ip4[:])
	} else {
		ip16 := cid.Addr.Addr().As16()
		rid.SetIpAddr(ip16[:])
	}
	rid.Port = cid.Addr.Port()
	rid.UniqueNumber = cid.ID
	return rid, nil
}

// ResolveString returns flagVal if non-empty, otherwise the value of envName.
func ResolveString(flagVal, envName string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envName)
}
