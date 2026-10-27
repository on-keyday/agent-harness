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
	// SessionStart and Stop carry ONLY the lifecycle reporter: the self-topic
	// subscription is seeded by the server (no SessionStart subscribe hook),
	// and a Stop hook that re-enters the turn conflicts with WakeStdin (spec
	// 2026-10-27-task-lifecycle D6: a report-and-exit Stop hook does not).
	for ev, want := range map[string]string{"SessionStart": "harness-cli lifecycle --hook claude idle", "Stop": "harness-cli lifecycle --hook claude idle"} {
		if got := hookCommands(t, hooks, ev); len(got) != 1 || got[0] != want {
			t.Errorf("%s hooks = %v, want only %q", ev, got, want)
		}
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
						// The cursor-era line. --since-last / --commit no
						// longer parse, so leaving it in place would make the
						// hook exit non-zero on every turn.
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent inbox --since-last --commit --json --user-prompt-submit-hook",
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

	// The retired Stop re-entry hook is pruned; the only Stop hook left is
	// the current report-and-exit lifecycle reporter.
	if got := hookCommands(t, hooks, "Stop"); len(got) != 1 || got[0] != "harness-cli lifecycle --hook claude idle" {
		t.Errorf("Stop hooks = %v, want only the lifecycle reporter", got)
	}

	// UserPromptSubmit: both cursor-era lines gone, current one added by
	// mergeHarnessHooks. The --commit form matters most: its flags no longer
	// parse, so a surviving copy would fail on every turn rather than quietly
	// doing the wrong thing.
	upGroups, _ := hooks["UserPromptSubmit"].([]any)
	if groupCommandSearch(upGroups, "harness-cli agent inbox --since-last --json") {
		t.Errorf("legacy UserPromptSubmit hook (no --commit) should be pruned")
	}
	if groupCommandSearch(upGroups, "harness-cli agent inbox --since-last --commit --json --user-prompt-submit-hook") {
		t.Errorf("cursor-era UserPromptSubmit hook should be pruned: its flags no longer parse")
	}
	if !groupCommandSearch(upGroups, "harness-cli agent inbox --json --user-prompt-submit-hook") {
		t.Errorf("current UserPromptSubmit hook missing after merge: %v", upGroups)
	}

	// Retired auto-subscribe hooks are pruned; the only SessionStart hook added
	// in their place is the lifecycle reporter (self-topic subscriptions are
	// server-seeded).
	startGroups, _ := hooks["SessionStart"].([]any)
	if groupCommandSearch(startGroups, "harness-cli agent subscribe --topic harness.hello") {
		t.Errorf("retired harness.hello SessionStart hook should be pruned: %v", startGroups)
	}
	if groupCommandSearch(startGroups, "harness-cli agent subscribe --self") {
		t.Errorf("retired --self SessionStart hook should be pruned: %v", startGroups)
	}
	if got := hookCommands(t, hooks, "SessionStart"); len(got) != 1 || got[0] != "harness-cli lifecycle --hook claude idle" {
		t.Errorf("SessionStart hooks = %v, want only the lifecycle reporter", got)
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

// hookCommands lists every command under hooks.<event>[].hooks[].
func hookCommands(t *testing.T, hooks map[string]any, event string) []string {
	t.Helper()
	var out []string
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		group, _ := g.(map[string]any)
		hs, _ := group["hooks"].([]any)
		for _, h := range hs {
			hook, _ := h.(map[string]any)
			cmd, _ := hook["command"].(string)
			out = append(out, cmd)
		}
	}
	return out
}

// lifecycleGroup returns the group under hooks.<event> whose hook runs cmd,
// with that hook.
func lifecycleGroup(t *testing.T, hooks map[string]any, event, cmd string) (group, hook map[string]any) {
	t.Helper()
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		gr, _ := g.(map[string]any)
		hs, _ := gr["hooks"].([]any)
		for _, h := range hs {
			hk, _ := h.(map[string]any)
			if c, _ := hk["command"].(string); c == cmd {
				return gr, hk
			}
		}
	}
	t.Fatalf("no %q hook under %s", cmd, event)
	return nil, nil
}

func readHooks(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	hooks, _ := parsed["hooks"].(map[string]any)
	return hooks
}

// The lifecycle reporters (spec 2026-10-27-task-lifecycle D9, D21, D22): every
// one async, so a dial to a remote server never delays the agent; two carry a
// matcher.
func TestWriteAgentSettings_LifecycleHooks(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	hooks := readHooks(t, dir)
	want := []struct{ event, matcher, cmd string }{
		{"SessionStart", "startup|resume|clear", "harness-cli lifecycle --hook claude idle"},
		{"UserPromptSubmit", "", "harness-cli lifecycle --hook claude working"},
		{"PreToolUse", "AskUserQuestion", "harness-cli lifecycle --hook claude waiting"},
		{"PostToolUse", "", "harness-cli lifecycle --hook claude working"},
		{"PermissionRequest", "", "harness-cli lifecycle --hook claude waiting"},
		{"Elicitation", "", "harness-cli lifecycle --hook claude waiting"},
		{"Stop", "", "harness-cli lifecycle --hook claude idle"},
		{"StopFailure", "", "harness-cli lifecycle --hook claude error"},
		// A failed tool still means the turn goes on; its is_interrupt marks
		// an Esc, which ends the turn with no Stop.
		{"PostToolUseFailure", "", "harness-cli lifecycle --hook claude working"},
		// claude ends the parent's turn right after launching a subagent; the
		// CLI tracks live subagents so that Stop does not read as idle.
		{"SubagentStart", "", "harness-cli lifecycle --hook claude working"},
		{"SubagentStop", "", "harness-cli lifecycle --hook claude idle"},
	}
	for _, w := range want {
		group, hook := lifecycleGroup(t, hooks, w.event, w.cmd)
		if hook["async"] != true {
			t.Errorf("%s %q: async = %v, want true", w.event, w.cmd, hook["async"])
		}
		if m, _ := group["matcher"].(string); m != w.matcher {
			t.Errorf("%s %q: matcher = %q, want %q", w.event, w.cmd, m, w.matcher)
		}
	}
	// The inbox hook keeps its place and stays synchronous: its stdout IS the
	// injected context.
	_, inbox := lifecycleGroup(t, hooks, "UserPromptSubmit", "harness-cli agent inbox --json --user-prompt-submit-hook")
	if _, ok := inbox["async"]; ok {
		t.Errorf("inbox hook gained an async key: %v", inbox)
	}
}

func TestWriteAgentSettings_LifecycleMergeIdempotentWithUserHooks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"hooks":{
		"PostToolUse":[{"hooks":[{"type":"command","command":"my-linter"}]}],
		"Stop":[{"hooks":[{"type":"command","command":"say done"}]}],
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"harness-cli agent inbox --json --user-prompt-submit-hook"}]}]}}`
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("second run changed the file:\n%s\n---\n%s", first, second)
	}
	hooks := readHooks(t, dir)
	count := func(event, cmd string) int {
		n := 0
		for _, c := range hookCommands(t, hooks, event) {
			if c == cmd {
				n++
			}
		}
		return n
	}
	if count("PostToolUse", "my-linter") != 1 || count("Stop", "say done") != 1 {
		t.Fatalf("user hooks lost: PostToolUse=%v Stop=%v", hookCommands(t, hooks, "PostToolUse"), hookCommands(t, hooks, "Stop"))
	}
	if count("PostToolUse", "harness-cli lifecycle --hook claude working") != 1 || count("UserPromptSubmit", "harness-cli lifecycle --hook claude working") != 1 ||
		count("Stop", "harness-cli lifecycle --hook claude idle") != 1 {
		t.Fatalf("lifecycle hooks not exactly once: %v", hooks)
	}
}
