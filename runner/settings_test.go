package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgentSettings_CreatesFileWithHook(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings.json missing: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	hooks, ok := parsed["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks key missing")
	}
	if _, ok := hooks["UserPromptSubmit"]; !ok {
		t.Error("UserPromptSubmit hook not present")
	}
	startGroups, ok := hooks["SessionStart"].([]any)
	if !ok || len(startGroups) == 0 {
		t.Fatal("SessionStart hook not present")
	}
	{
		g0, _ := startGroups[0].(map[string]any)
		hs, _ := g0["hooks"].([]any)
		if len(hs) == 0 {
			t.Fatal("SessionStart hook group has no hooks")
		}
		h0, _ := hs[0].(map[string]any)
		cmd, _ := h0["command"].(string)
		if !strings.Contains(cmd, "agent subscribe") || !strings.Contains(cmd, "harness.hello") {
			t.Errorf("SessionStart hook command missing expected pieces: %q", cmd)
		}
		if strings.Contains(cmd, "/dev/null") {
			t.Errorf("SessionStart hook uses POSIX-only redirect; breaks on Windows shells: %q", cmd)
		}
	}
	if _, ok := hooks["Stop"]; ok {
		t.Error("Stop hook must not be present: it conflicts with WakeStdin stdin injection")
	}
	perms, ok := parsed["permissions"].(map[string]any)
	if !ok {
		t.Fatal("permissions key missing")
	}
	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) == 0 {
		t.Fatal("permissions.allow missing or empty")
	}
	found := false
	for _, v := range allow {
		if s, _ := v.(string); s == "Bash(harness-cli *)" {
			found = true
		}
	}
	if !found {
		t.Errorf("permissions.allow missing Bash(harness-cli *), got %v", allow)
	}
}

// TestWriteAgentSettings_MergesWithExisting verifies the merge semantics:
// pre-existing user keys/hooks/permissions survive the call, the runner's
// own hooks and allow-entry are added, and a second call is a no-op
// (idempotent) so repeated runs on a resumed worktree don't accumulate
// duplicates.
func TestWriteAgentSettings_MergesWithExisting(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "echo user-hook"},
					},
				},
			},
			"PostToolUse": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "echo user-post"},
					},
				},
			},
		},
		"permissions": map[string]any{
			"allow": []any{"Read(*)"},
			"deny":  []any{"Bash(rm -rf *)"},
		},
		"customKey": "preserve me",
	}
	raw, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(sub, "settings.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	first := readSettings(t, dir)

	// Foreign keys preserved
	if first["customKey"] != "preserve me" {
		t.Errorf("customKey lost: got %v", first["customKey"])
	}
	hooks := first["hooks"].(map[string]any)
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("foreign hook event PostToolUse lost")
	}
	perms := first["permissions"].(map[string]any)
	allow := perms["allow"].([]any)
	if !containsString(allow, "Read(*)") {
		t.Error("foreign permissions.allow entry Read(*) lost")
	}
	if _, ok := perms["deny"]; !ok {
		t.Error("foreign permissions.deny lost")
	}

	// Foreign SessionStart hook preserved alongside the harness hook
	startGroups := hooks["SessionStart"].([]any)
	if !groupCommandSearch(startGroups, "echo user-hook") {
		t.Error("user SessionStart hook was overwritten")
	}
	if !groupCommandSearch(startGroups, "harness-cli agent subscribe --topic harness.hello") {
		t.Error("harness SessionStart hook missing after merge")
	}

	// Harness allow entry was appended (not replacing user's Read(*))
	if !containsString(allow, "Bash(harness-cli *)") {
		t.Error("harness allow entry missing after merge")
	}

	// Idempotency: second call must not duplicate.
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	second := readSettings(t, dir)
	secondHooks := second["hooks"].(map[string]any)["SessionStart"].([]any)
	if countGroupCommand(secondHooks, "harness-cli agent subscribe --topic harness.hello") != 1 {
		t.Errorf("harness SessionStart hook duplicated after second call: %v", secondHooks)
	}
	secondAllow := second["permissions"].(map[string]any)["allow"].([]any)
	if countString(secondAllow, "Bash(harness-cli *)") != 1 {
		t.Errorf("harness allow entry duplicated after second call: %v", secondAllow)
	}
}

// TestWriteAgentSettings_RejectsMalformed asserts the merge code surfaces
// a parse error rather than silently overwriting a corrupt user file.
func TestWriteAgentSettings_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err == nil {
		t.Fatal("expected error on malformed settings.json")
	}
}

func readSettings(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func containsString(xs []any, want string) bool {
	for _, v := range xs {
		if s, _ := v.(string); s == want {
			return true
		}
	}
	return false
}

func countString(xs []any, want string) int {
	n := 0
	for _, v := range xs {
		if s, _ := v.(string); s == want {
			n++
		}
	}
	return n
}

func groupCommandSearch(groups []any, cmd string) bool {
	return countGroupCommand(groups, cmd) > 0
}

func countGroupCommand(groups []any, cmd string) int {
	n := 0
	for _, g := range groups {
		group, _ := g.(map[string]any)
		hs, _ := group["hooks"].([]any)
		for _, h := range hs {
			hook, _ := h.(map[string]any)
			if c, _ := hook["command"].(string); c == cmd {
				n++
			}
		}
	}
	return n
}
