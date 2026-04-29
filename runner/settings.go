package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// agentSettings is the schema written to <worktree>/.claude/settings.json.
type agentSettings struct {
	Hooks map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
	Hooks []hookSpec `json:"hooks"`
}

type hookSpec struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// WriteAgentSettings creates <dir>/.claude/settings.json with three hooks:
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
//     prompt submission (covers idle agents woken by user input).
//   - Stop: emits a Claude Code block decision when new agentboard messages
//     arrived during the just-finished turn, so the agent re-enters and
//     processes them without requiring a user prompt.
//
// The shared --since-last cursor at ~/.cache/harness/agent-cursor-<task>
// prevents the same seq from being delivered twice across the two paths.
func WriteAgentSettings(worktreeDir string) error {
	s := agentSettings{
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
					Command: "harness-cli agent inbox --since-last --json",
				}},
			}},
			"Stop": {{
				Hooks: []hookSpec{{
					Type:    "command",
					Command: "harness-cli agent inbox --since-last --stop-hook",
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
