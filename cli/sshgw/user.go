//go:build !js

// Package sshgw serves SSH connections that land in harness interactive
// sessions, so an ordinary `ssh` client — an ~/.ssh/config alias, tmux, mosh,
// a script — can reach a task without a harness binary on that host.
//
// The listener runs inside an ordinary harness client (the `harness-cli
// ssh-gateway` verb, or the TUI): the server gains no second authentication
// surface, and this speaks only the existing AttachSession RPC.
//
// Design: docs/superpowers/specs/2026-08-25-ssh-gateway-design.md
package sshgw

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const taskIDHexLen = 32

// ParseUserName maps an ssh user name to the task it names and the attach mode
// it asks for:
//
//	<32-hex>            cowrite — types, takes no seat
//	<32-hex>.control    control — takes the seat, owns the PTY size
//	<32-hex>.view       view    — reads only
//
// The bare form is cowrite on purpose. A control attach is a takeover
// server-side (SessionMux.Attach closes the previous controller's stream), so a
// bare `ssh <id>@host` would silently detach whatever the operator had attached
// in the TUI. Arriving somewhere should not evict you from it; the takeover
// stays available, spelled out.
//
// There is no `.cowrite` spelling: the bare form already is one, and a second
// spelling of the same thing would show up as two different names in anything
// that logs this gateway.
func ParseUserName(name string) (string, protocol.AttachMode, error) {
	id, suffix, hasSuffix := strings.Cut(name, ".")
	if !isTaskIDHex(id) {
		return "", 0, fmt.Errorf("ssh user name %q is not a task id: use <32-hex-task-id>[.control|.view]@host (lowercase hex; no suffix means cowrite)", name)
	}
	mode := protocol.AttachMode_Cowrite
	if hasSuffix {
		switch suffix {
		case "control":
			mode = protocol.AttachMode_Control
		case "view":
			mode = protocol.AttachMode_View
		default:
			return "", 0, fmt.Errorf("ssh user name %q: unknown mode %q (want .control or .view, or no suffix for cowrite)", name, suffix)
		}
	}
	return id, mode, nil
}

// isTaskIDHex reports whether s is exactly 32 LOWERCASE hex characters.
//
// Lowercase-only rather than case-insensitive: every surface in this system
// prints ids lowercase, so an uppercase name is a typo rather than a request,
// and accepting both would let one session appear under two names.
func isTaskIDHex(s string) bool {
	if len(s) != taskIDHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
