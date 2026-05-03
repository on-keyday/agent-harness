package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// hookSpec describes one item under settings.json's
// `hooks.<event>[].hooks[]`. The wider config can carry additional fields
// the harness does not control; merge logic preserves those by working
// against `map[string]any` rather than this typed view.
type hookSpec struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// harnessHookEntries enumerates the hooks the runner injects. The merge
// logic uses (event, command) as the de-duplication key — running
// WriteAgentSettings repeatedly on the same dir adds nothing new after
// the first call.
var harnessHookEntries = []struct {
	Event   string
	Command string
}{
	{"SessionStart", "harness-cli agent subscribe --topic harness.hello"},
	{"SessionStart", "harness-cli agent subscribe --self"},
	{"UserPromptSubmit", "harness-cli agent inbox --since-last --commit --json"},
}

// harnessAllowEntry is the single permissions.allow entry the runner
// injects so the agent can call harness-cli freely.
const harnessAllowEntry = "Bash(harness-cli *)"

// WriteAgentSettings ensures <dir>/.claude/settings.json carries the
// runner's hooks and permission allow-entry, merging into any existing
// content rather than overwriting it.
//
// Hooks injected:
//   - SessionStart × 2: subscribes the agent to (a) the reserved handshake
//     topic `harness.hello` for multi-agent discovery, and (b) the agent's
//     own inbound topic `chat.<first-8-hex-of-task-id>` (resolved by
//     `harness-cli agent subscribe --self` from HARNESS_TASK_ID). Both
//     follow the SKILL.md convention so the agent does not need to
//     remember to subscribe. Subscriptions are keyed by (rid, tid) on the
//     broker and de-duplicated via a map, so re-firing this hook on
//     /clear or /resume is a no-op. The commands are left bare (no shell
//     redirect or expansion) for cross-OS portability — `>/dev/null` is
//     not valid in PowerShell / cmd, and the runner OS is not pinned;
//     this is also why --self is resolved by the CLI rather than by
//     shell-substituting $HARNESS_TASK_ID, whose syntax differs per
//     shell. The resulting `{"status":"ok"}` lines are injected as
//     additionalContext at session start; the noise is negligible.
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
// Merge semantics: existing top-level keys (e.g. user-defined hooks under
// other events, custom permissions, anything outside `hooks` and
// `permissions`) are preserved. The runner's hookSpec is appended to
// `hooks.<event>` only if no entry with the same command is already
// present, and `permissions.allow` is treated the same way. Calling this
// twice is a no-op the second time.
//
// On a malformed existing file, the call returns an error rather than
// silently clobbering — the worktree is then resumable once the user fixes
// the file by hand. Resume idempotency in WorktreeManager.Create means a
// retry just picks up where this failure left off.
//
// The --since-last cursor at ~/.cache/harness/agent-cursor-<task>
// prevents the same seq from being delivered twice. The UserPromptSubmit
// hook passes --commit to advance the live cursor; manual
// `harness-cli agent inbox --since-last` callers (LLM-initiated) leave
// it off and read from the prev-cursor snapshot — i.e. they see the
// same batch the most recent hook just delivered, idempotently. See
// cli/agent/cursor.go.
func WriteAgentSettings(worktreeDir string) error {
	sub := filepath.Join(worktreeDir, ".claude")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return err
	}
	path := filepath.Join(sub, "settings.json")

	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("settings.json malformed: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	mergeHarnessHooks(root)
	mergeHarnessAllow(root)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// mergeHarnessHooks adds each harnessHookEntries item under root["hooks"],
// creating intermediate maps/slices on the fly. An entry whose command
// already appears anywhere under hooks.<event>[].hooks[] is treated as
// already-installed and skipped, regardless of which group it lives in.
// This makes repeated WriteAgentSettings calls idempotent and tolerates
// users who hand-merged the runner's hook into their own group.
func mergeHarnessHooks(root map[string]any) {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, entry := range harnessHookEntries {
		groups, _ := hooks[entry.Event].([]any)
		if hookCommandPresent(groups, entry.Command) {
			hooks[entry.Event] = groups
			continue
		}
		groups = append(groups, map[string]any{
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": entry.Command,
				},
			},
		})
		hooks[entry.Event] = groups
	}
	root["hooks"] = hooks
}

// hookCommandPresent reports whether any hookSpec inside the given
// settings.json hook-group list carries the given command string.
func hookCommandPresent(groups []any, command string) bool {
	for _, g := range groups {
		group, _ := g.(map[string]any)
		hs, _ := group["hooks"].([]any)
		for _, h := range hs {
			hook, _ := h.(map[string]any)
			if cmd, _ := hook["command"].(string); cmd == command {
				return true
			}
		}
	}
	return false
}

// mergeHarnessAllow appends harnessAllowEntry to permissions.allow if not
// already present. Other permission keys (deny, defaultMode, …) and other
// allow-entries are preserved.
func mergeHarnessAllow(root map[string]any) {
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	for _, v := range allow {
		if s, _ := v.(string); s == harnessAllowEntry {
			perms["allow"] = allow
			root["permissions"] = perms
			return
		}
	}
	perms["allow"] = append(allow, harnessAllowEntry)
	root["permissions"] = perms
}
