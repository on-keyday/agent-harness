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

// UserOpts is everything an ssh user name asks for beyond naming the task.
type UserOpts struct {
	// Mode is how a SHELL session attaches. Ignored by every channel that
	// does not attach — an exec and a forward both reach the task without
	// taking a seat.
	Mode protocol.AttachMode

	// Detach makes an exec on this connection leave whatever it starts
	// running after the command ends. Off by default, because the default is
	// right for `ssh <id>@gw 'make test'`: interrupting that must stop make's
	// children too.
	Detach bool

	// SshdParent runs an exec's shell line under a process named sshd, for a
	// client that checks its own ancestry by process name. Windows runners
	// only; elsewhere the runner refuses the exec rather than running it
	// without the property.
	SshdParent bool
}

// ParseUserName maps an ssh user name to the task it names and what the
// connection is asking for:
//
//	<32-hex>                      cowrite — types, takes no seat
//	<32-hex>.control              control — takes the seat, owns the PTY size
//	<32-hex>.view                 view    — reads only
//	<32-hex>.detach               execs leave what they start running
//	<32-hex>.detach,sshd-parent   …and run under a parent named sshd
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
//
// The suffix is a comma-separated LIST, and it carries two kinds of thing: at
// most one attach mode, plus any number of exec options. That mixing is
// deliberate and worth the note, because this file used to say the suffix
// selects an attach mode and nothing else.
//
// D3 and D11 (both operator decisions) fix that the user name selects the task
// and that bare/.control/.view are the attach forms. Neither says the suffix
// may carry nothing else. What argued against it was tcpip.go's note that
// treating `.view` as read-only for a forward "would advertise an authority
// boundary the gateway does not have" — and that is an argument about
// PERMISSIONS, which neither of these options is. They change how a command
// runs, not what the connection is allowed to reach; anyone who got this far
// already holds the operator's credentials either way.
//
// A flat list rather than a second axis because the two kinds never collide in
// practice: an attach mode governs a shell channel, and these options govern
// exec channels, which never attach. `.control,detach` is accepted anyway — a
// connection may open both kinds of channel, and refusing the combination
// would be a constraint invented here rather than one anything needs.
//
// Where it is meant to be typed is an ~/.ssh/config `User` line, which is the
// only thing that reaches this gateway from a client that builds its own ssh
// invocation.
func ParseUserName(name string) (string, UserOpts, error) {
	const forms = "use <32-hex-task-id>[.<opt>[,<opt>...]]@host, where opt is control | view | detach | sshd-parent (lowercase hex; no suffix means cowrite)"
	id, suffix, hasSuffix := strings.Cut(name, ".")
	if !isTaskIDHex(id) {
		return "", UserOpts{}, fmt.Errorf("ssh user name %q is not a task id: %s", name, forms)
	}
	opts := UserOpts{Mode: protocol.AttachMode_Cowrite}
	if !hasSuffix {
		return id, opts, nil
	}
	modeSeen := false
	for _, tok := range strings.Split(suffix, ",") {
		switch tok {
		case "control", "view":
			// One attach mode, because two would have to be resolved by a
			// precedence rule nobody typed and the loser would be silent.
			if modeSeen {
				return "", UserOpts{}, fmt.Errorf("ssh user name %q names more than one attach mode: %s", name, forms)
			}
			modeSeen = true
			if tok == "control" {
				opts.Mode = protocol.AttachMode_Control
			} else {
				opts.Mode = protocol.AttachMode_View
			}
		case "detach":
			opts.Detach = true
		case "sshd-parent":
			opts.SshdParent = true
		default:
			return "", UserOpts{}, fmt.Errorf("ssh user name %q: unknown option %q: %s", name, tok, forms)
		}
	}
	return id, opts, nil
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
