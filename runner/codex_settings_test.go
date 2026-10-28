package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgentSettings_CodexMerge(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Inline user hooks remain in config.toml; Codex loads both sources.
	config := []byte("[hooks]\n# user-owned configuration\n")
	if err := os.WriteFile(filepath.Join(sub, "config.toml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "hooks.json")
	existing := `{"description":"user hooks","hooks":{
		"PostToolUse":[{"matcher":"Bash","hooks":[
			{"type":"command","command":"my-linter"},
			{"type":"command","command":"harness-cli retired-command"}]}],
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"harness-cli agent inbox --json --user-prompt-submit-hook"}]}]
	}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("repeat injection changed Codex hooks: %v", err)
	}
	gotConfig, err := os.ReadFile(filepath.Join(sub, "config.toml"))
	if err != nil || !bytes.Equal(config, gotConfig) {
		t.Fatalf("config.toml changed: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(first, &root); err != nil {
		t.Fatal(err)
	}
	if root["description"] != "user hooks" || root["permissions"] != nil {
		t.Fatalf("user metadata lost or Claude permissions copied: %v", root)
	}
	hooks := root["hooks"].(map[string]any)
	if got := hookCommands(t, hooks, "PostToolUse"); len(got) != 2 || got[0] != "my-linter" || got[1] != "harness-cli lifecycle --hook codex working" {
		t.Fatalf("user hook lost, retired hook retained, or lifecycle missing: %v", got)
	}
	for _, event := range []string{"StopFailure", "PostToolUseFailure", "Elicitation"} {
		if _, exists := hooks[event]; exists {
			t.Errorf("Claude-only event injected: %s", event)
		}
	}
	// Expected behavior, independent of the implementation's entry list.
	want := map[string]string{
		"SessionStart": "idle", "UserPromptSubmit": "working",
		"PreToolUse": "waiting", "PermissionRequest": "waiting",
		"PostToolUse": "working", "Stop": "idle", "Interrupt": "idle",
		"SessionEnd": "idle", "SubagentStart": "working", "SubagentStop": "idle",
	}
	for event, state := range want {
		group, hook := lifecycleGroup(t, hooks, event, "harness-cli lifecycle --hook codex "+state)
		if event != "SessionEnd" && hook["async"] != true {
			t.Errorf("%s reporter is not asynchronous", event)
		}
		if (event == "SessionEnd" || event == "Interrupt") && hook["timeout"] != float64(3) {
			t.Errorf("%s must respect Codex's three-second limit: %v", event, hook["timeout"])
		}
		matcher, _ := group["matcher"].(string)
		if event == "SessionStart" && matcher != "startup|resume|clear" {
			t.Errorf("SessionStart may reset state mid-turn: matcher=%q", matcher)
		}
		if event == "PreToolUse" && matcher != "^request_user_input$" {
			t.Errorf("PreToolUse must only mark user questions waiting: %q", matcher)
		}
	}
	if got := hookCommands(t, hooks, "UserPromptSubmit"); len(got) != 2 {
		t.Fatalf("expected one inbox and one lifecycle hook: %v", got)
	}
	_, inbox := lifecycleGroup(t, hooks, "UserPromptSubmit", "harness-cli agent inbox --json --user-prompt-submit-hook")
	if inbox["async"] == true {
		t.Fatal("inbox must deliver context synchronously")
	}
}

func TestWriteAgentSettings_CodexRejectsMalformedWithoutOverwriting(t *testing.T) {
	for _, original := range []string{
		`{`, `null`, `[]`, `{"hooks":null}`, `{"hooks":[]}`,
		`{"hooks":{"Stop":{}}}`, `{"hooks":{"Stop":[null]}}`,
		`{"hooks":{"Stop":[{"hooks":{}}]}}`,
	} {
		t.Run(original, func(t *testing.T) {
			dir := t.TempDir()
			sub := filepath.Join(dir, ".codex")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sub, "hooks.json")
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := WriteAgentSettings(dir); err == nil || !strings.Contains(err.Error(), "hooks.json") {
				t.Fatalf("expected actionable Codex config error, got %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != original {
				t.Fatalf("malformed user file overwritten: %s (%v)", got, err)
			}
		})
	}
}

func TestWriteAgentSettings_CodexIndependentOfMalformedClaudeSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err == nil {
		t.Fatal("malformed Claude settings must still be reported")
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex", "hooks.json")); err != nil {
		t.Fatalf("malformed Claude settings prevented Codex injection: %v", err)
	}
}

func TestCodexInjectedPathsDoNotHideUserConfig(t *testing.T) {
	dirty := filterDirtyPaths([]byte("?? .codex/hooks.json\x00?? .codex/config.toml\x00"), HarnessInjectedPaths)
	if len(dirty) != 1 || dirty[0] != ".codex/config.toml" {
		t.Fatalf("user Codex configuration must prevent worktree cleanup: %v", dirty)
	}
}
