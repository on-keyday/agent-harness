package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if _, ok := hooks["SessionStart"]; ok {
		t.Error("SessionStart hook must not be present: self-topic subscription is seeded by the server")
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

	// Foreign SessionStart hook preserved; harness no longer injects one.
	startGroups := hooks["SessionStart"].([]any)
	if !groupCommandSearch(startGroups, "echo user-hook") {
		t.Error("user SessionStart hook was overwritten")
	}
	if groupCommandSearch(startGroups, "harness-cli agent subscribe --self") {
		t.Error("harness SessionStart hook should not be added after merge")
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
	secondHooks := second["hooks"].(map[string]any)
	secondStartGroups := secondHooks["SessionStart"].([]any)
	if countGroupCommand(secondStartGroups, "harness-cli agent subscribe --self") != 0 {
		t.Errorf("harness --self SessionStart hook must not be present after second call: %v", secondStartGroups)
	}
	secondAllow := second["permissions"].(map[string]any)["allow"].([]any)
	if countString(secondAllow, "Bash(harness-cli *)") != 1 {
		t.Errorf("harness allow entry duplicated after second call: %v", secondAllow)
	}
}

// TestWriteAgentSettings_PrunesRetiredHarnessHooks verifies that a hook
// command starting with the harness prefix but no longer present in
// harnessHookEntries is removed on the next WriteAgentSettings call.
// Concretely covers the legacy Stop hook (retired when WakeStdin replaced
// the Stop-based re-entry) and the older UserPromptSubmit variant without
// --commit (superseded by the --commit form). Without prune, both would
// persist forever in any worktree initialised by an older runner and
// re-fire on every turn, redelivering the same seqs.
func TestWriteAgentSettings_PrunesRetiredHarnessHooks(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent inbox --since-last --stop-hook",
						},
					},
				},
			},
			// A worktree initialised by an older runner that still
			// auto-subscribed in SessionStart. These are now retired from
			// harnessHookEntries, so they must be pruned on this call.
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent subscribe --topic harness.hello",
						},
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent subscribe --self",
						},
					},
				},
			},
			"UserPromptSubmit": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent inbox --since-last --json",
						},
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
	}
	raw, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(sub, "settings.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)
	hooks, _ := got["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatal("hooks key missing after prune+merge")
	}

	// Retired Stop event group should be gone entirely (no current entries
	// repopulate it, so the empty event is removed).
	if _, ok := hooks["Stop"]; ok {
		t.Errorf("Stop event should be pruned, got %v", hooks["Stop"])
	}

	// UserPromptSubmit: legacy `--json` (no --commit) gone, current
	// `--commit --json` added by mergeHarnessHooks.
	upGroups, _ := hooks["UserPromptSubmit"].([]any)
	if groupCommandSearch(upGroups, "harness-cli agent inbox --since-last --json") {
		t.Errorf("legacy UserPromptSubmit hook (no --commit) should be pruned")
	}
	if !groupCommandSearch(upGroups, "harness-cli agent inbox --since-last --commit --json") {
		t.Errorf("current UserPromptSubmit hook missing after merge")
	}

	// Retired auto-subscribe hooks are pruned; no current SessionStart hook is
	// added in their place because self-topic subscriptions are server-seeded.
	startGroups, _ := hooks["SessionStart"].([]any)
	if groupCommandSearch(startGroups, "harness-cli agent subscribe --topic harness.hello") {
		t.Errorf("retired harness.hello SessionStart hook should be pruned: %v", startGroups)
	}
	if groupCommandSearch(startGroups, "harness-cli agent subscribe --self") {
		t.Errorf("retired --self SessionStart hook should be pruned: %v", startGroups)
	}
	if _, ok := hooks["SessionStart"]; ok {
		t.Errorf("SessionStart event should be removed after stale hooks are pruned: %v", startGroups)
	}

	// User-authored hook in a foreign event must survive.
	postGroups, _ := hooks["PostToolUse"].([]any)
	if !groupCommandSearch(postGroups, "echo user-post") {
		t.Errorf("user PostToolUse hook lost: %v", postGroups)
	}
}

// TestWriteAgentSettings_PrunePreservesUserAuthoredHarnessLikeHooks: even if
// a user manually adds a `harness-cli ...` command that matches the prefix
// but happens to also be in the current harnessHookEntries (e.g. by
// hand-merging), we must not double-delete it. Conversely, a non-prefix
// user hook that *coexists in the same group* as a stale harness hook must
// be preserved.
func TestWriteAgentSettings_PrunePreservesGroupSiblings(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent inbox --since-last --stop-hook",
						},
						map[string]any{
							"type":    "command",
							"command": "echo user-stop-hook",
						},
					},
				},
			},
		},
	}
	raw, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(sub, "settings.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)
	hooks, _ := got["hooks"].(map[string]any)
	stopGroups, _ := hooks["Stop"].([]any)
	if !groupCommandSearch(stopGroups, "echo user-stop-hook") {
		t.Errorf("user-authored Stop hook lost when its sibling was pruned: %v", stopGroups)
	}
	if groupCommandSearch(stopGroups, "harness-cli agent inbox --since-last --stop-hook") {
		t.Errorf("retired harness hook should have been pruned: %v", stopGroups)
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
