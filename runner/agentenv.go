package runner

import (
	"encoding/hex"

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
	}
	if s.Hostname != "" {
		env = append(env, "HARNESS_HOSTNAME="+s.Hostname)
	}
	return env
}
