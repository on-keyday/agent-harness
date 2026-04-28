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

// WriteAgentSettings creates <dir>/.claude/settings.json with the
// UserPromptSubmit hook that injects pending agentboard messages each turn.
func WriteAgentSettings(worktreeDir string) error {
	s := agentSettings{
		Hooks: map[string][]hookGroup{
			"UserPromptSubmit": {{
				Hooks: []hookSpec{{
					Type:    "command",
					Command: "harness-cli agent inbox --since-last --json",
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
