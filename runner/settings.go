package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// agentSettings is the schema written to <worktree>/.claude/settings.json.
type agentSettings struct {
	Hooks       map[string][]hookGroup `json:"hooks"`
	Permissions agentPermissions       `json:"permissions"`
}

type agentPermissions struct {
	Allow []string `json:"allow"`
}

type hookGroup struct {
	Hooks []hookSpec `json:"hooks"`
}

type hookSpec struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// WriteAgentSettings creates <dir>/.claude/settings.json with two hooks:
//   - SessionStart: subscribes the agent to the reserved handshake topic
//     `harness.hello` so the multi-agent meeting protocol works without the
//     agent having to remember to subscribe. Subscriptions are keyed by
//     (rid, tid) on the broker and de-duplicated via a map, so re-firing
//     this hook on /clear or /resume is a no-op. The command is left bare
//     (no shell redirect) for cross-OS portability — `>/dev/null` is not
//     valid in PowerShell / cmd, and the runner OS is not pinned. The
//     resulting `{"status":"ok"}` line is injected as additionalContext at
//     session start; the noise is negligible (17 bytes, once per session).
//   - UserPromptSubmit: injects pending agentboard messages on each user
//     prompt submission. This covers both normal user turns and the
//     synthetic wake prompt written by Session.WakeStdin when the runner
//     detects new agentboard messages while the agent is idle.
//
// There is deliberately no Stop hook. A Stop-hook-based re-entry blocks
// Claude Code's stdin while the agent continues its current turn, which
// prevents the WakeStdin stdin injection (the "<harness:agentboard-wake>"
// marker) from being delivered as a clean new turn. The two mechanisms
// conflict: Stop hook rejection wins, the wake prompt is deferred until
// the agent finally exits, and the agent ends up processing messages in
// an awkward autonomous chain rather than in the context of a user turn.
// WakeStdin alone is sufficient.
//
// The --since-last cursor at ~/.cache/harness/agent-cursor-<task>
// prevents the same seq from being delivered twice. The UserPromptSubmit
// hook passes --commit to advance the live cursor; manual
// `harness-cli agent inbox --since-last` callers (LLM-initiated) leave
// it off and read from the prev-cursor snapshot — i.e. they see the
// same batch the most recent hook just delivered, idempotently. See
// cli/agent/cursor.go.
func WriteAgentSettings(worktreeDir string) error {
	s := agentSettings{
		Permissions: agentPermissions{
			Allow: []string{"Bash(harness-cli *)"},
		},
		Hooks: map[string][]hookGroup{
			"SessionStart": {{
				Hooks: []hookSpec{{
					Type:    "command",
					Command: "harness-cli agent subscribe --topic harness.hello",
				}},
			}},
			"UserPromptSubmit": {{
				Hooks: []hookSpec{{
					Type:    "command",
					Command: "harness-cli agent inbox --since-last --commit --json",
				}},
			}},
		},
	}
	sub := filepath.Join(worktreeDir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sub, "settings.json"), data, 0o644)
}
