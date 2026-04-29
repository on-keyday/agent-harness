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

// WriteAgentSettings creates <dir>/.claude/settings.json with two hooks:
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
