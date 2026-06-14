package runner

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AgentEnvSpec is the input bundle for BuildAgentEnv.
type AgentEnvSpec struct {
	ServerCID  objproto.ConnectionID
	RunnerID   objproto.ConnectionID
	TaskID     protocol.TaskID
	RepoPath   string
	Hostname   string
	WSPath     string
	AuthTicket [16]byte
	// BinDir, when non-empty, is prepended to PATH so the agent can invoke
	// harness-cli without a fully-qualified path. The agent runs in a
	// per-task worktree distinct from the runner's binary directory, so
	// PATH inheritance from the runner alone does not surface harness-cli.
	BinDir string
	// PSK, when non-nil, is forwarded to the agent subprocess as
	// HARNESS_PSK so harness-cli invocations from inside the agent
	// (e.g. agentboard send/recv) can authenticate against the
	// PSK-protected server.
	PSK []byte

	// ProxyVia, when non-empty, is the runner's listen-side ConnectionID
	// string (e.g. "ws:127.0.0.1:8540-*"). Set in listen mode. Injected as
	// HARNESS_PROXY_VIA_RUNNER so agent processes use the objproto
	// negotiated-proxy path (Phase B) instead of dialing the server directly.
	ProxyVia string

	// X11Enabled injects DISPLAY=127.0.0.1:<X11Display> even when there is no
	// XAUTHORITY (no-auth forwarding). XAUTHORITY is emitted only when
	// X11AuthFile != "".
	X11Enabled bool
	// X11Display/X11AuthFile: when X11Enabled, DISPLAY=127.0.0.1:<X11Display>
	// is always injected. XAUTHORITY=<X11AuthFile> is injected only when
	// X11AuthFile != "" (cookie-authenticated forwarding). See runner/x11.go.
	X11Display  int
	X11AuthFile string
}

// BuildAgentEnv returns "KEY=VAL" entries to merge with os.Environ() in Process.Env.
// Empty Hostname is omitted (HARNESS_HOSTNAME absent rather than set to empty).
func BuildAgentEnv(s AgentEnvSpec) []string {
	env := []string{
		"HARNESS_SERVER_CID=" + s.ServerCID.String(),
		"HARNESS_RUNNER_ID=" + s.RunnerID.String(),
		"HARNESS_TASK_ID=" + hex.EncodeToString(s.TaskID.Id[:]),
		"HARNESS_REPO_PATH=" + s.RepoPath,
		"HARNESS_WS_PATH=" + s.WSPath,
		"HARNESS_AUTH_TICKET=" + hex.EncodeToString(s.AuthTicket[:]),
		// Disable MSYS/MinGW automatic POSIX-path → Windows-path rewriting
		// so that POSIX-style args we pass (topics like "chat/demo", ws
		// paths like "/ws", task IDs starting with "/", etc.) are not
		// silently mangled when claude runs under MSYS bash on Windows.
		// Both vars are no-ops outside MSYS/MinGW.
		"MSYS_NO_PATHCONV=1",
		"MSYS2_ARG_CONV_EXCL=*",
	}
	if s.Hostname != "" {
		env = append(env, "HARNESS_HOSTNAME="+s.Hostname)
	}
	if s.BinDir != "" {
		path := s.BinDir
		if existing := os.Getenv("PATH"); existing != "" {
			path += string(os.PathListSeparator) + existing
		}
		env = append(env, "PATH="+path)
	}
	if len(s.PSK) > 0 {
		env = append(env, "HARNESS_PSK="+string(s.PSK))
	}
	if s.ProxyVia != "" {
		env = append(env, "HARNESS_PROXY_VIA_RUNNER="+rewriteProxyViaForLocalDial(s.ProxyVia))
	}
	if s.X11Enabled {
		env = append(env, fmt.Sprintf("DISPLAY=127.0.0.1:%d", s.X11Display))
		if s.X11AuthFile != "" {
			env = append(env, "XAUTHORITY="+s.X11AuthFile)
		}
	}
	return env
}

// rewriteProxyViaForLocalDial converts a runner-listen-side bind address
// inside a ConnectionID string into one suitable for the agent (same host)
// to dial.
//
// ProxyVia is auto-derived from `--listen` in listen.go (e.g. WSListen
// "0.0.0.0:8540" → "ws:0.0.0.0:8540-*"), but bind addresses are not
// portable dial targets:
//   - 0.0.0.0 / 127.0.0.0/8 — Linux kernel accepts 0.0.0.0 dial as localhost
//     but Windows / macOS reject it. Rewrite to 127.0.0.1.
//   - :: / [::] — IPv6 unspecified; rewrite to ::1.
//   - empty host (":8540" → "ws::8540-*") — DNS lookup of "" fails on every
//     OS. Rewrite to 127.0.0.1.
//   - already a specific IP / hostname — pass through unchanged.
//
// We only touch the host portion; transport, port, and connection_id are
// preserved.
func rewriteProxyViaForLocalDial(via string) string {
	// Parse `<transport>:<addr>-<id>` minimally. ConnectionID strings use
	// "<transport>:<addrport>-<id>" where addrport may be either
	// "host:port" or "[ipv6]:port". We split off the leading
	// "<transport>:" prefix, then split off the trailing "-<id>" suffix,
	// leaving the addrport to rewrite.
	transportSep := strings.IndexByte(via, ':')
	if transportSep < 0 {
		return via
	}
	transport, rest := via[:transportSep], via[transportSep+1:]
	lastDash := strings.LastIndexByte(rest, '-')
	if lastDash < 0 {
		return via
	}
	addrPort, idSuffix := rest[:lastDash], rest[lastDash:]

	// Find the host:port split. With IPv6 bracketed form ("[::]:8540"),
	// the colon between host and port is after ']'. Otherwise just last ':'.
	var host, port string
	if strings.HasPrefix(addrPort, "[") {
		end := strings.IndexByte(addrPort, ']')
		if end < 0 {
			return via // malformed; leave as-is
		}
		host = addrPort[1:end]    // inside brackets
		port = addrPort[end+1:]   // includes leading ':'
	} else {
		i := strings.LastIndexByte(addrPort, ':')
		if i < 0 {
			return via
		}
		host = addrPort[:i]
		port = addrPort[i:] // includes leading ':'
	}

	// Rewrite host if it is a bind-only / unspecified address.
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}

	// Reassemble. IPv6 hosts need brackets when paired with a port suffix.
	if strings.ContainsRune(host, ':') {
		return transport + ":[" + host + "]" + port + idSuffix
	}
	return transport + ":" + host + port + idSuffix
}
