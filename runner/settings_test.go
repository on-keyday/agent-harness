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

func TestWriteAgentSettings_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(sub, "settings.json"))
	if len(data) <= 2 {
		t.Errorf("expected non-empty content, got %q", data)
	}
}
