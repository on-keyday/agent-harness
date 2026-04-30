package runner

import (
	"encoding/hex"
	"os"

	"github.com/on-keyday/agent-harness/objproto"
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
	return env
}
