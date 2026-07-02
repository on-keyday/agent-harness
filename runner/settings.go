package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// harnessHookPrefix marks any hook command the runner has ever injected.
// The runner is the only producer of hook commands starting with
// "harness-cli " — single-user dogfood deployment, no other source ever
// writes such hooks. pruneStaleHarnessHooks uses this prefix to identify
// hooks the runner once injected so it can drop the ones no longer in
// harnessHookEntries (e.g. a Stop hook that an older runner installed but
// the current code has retired).
const harnessHookPrefix = "harness-cli "

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
//   - UserPromptSubmit: injects pending agentboard messages on each user
//     prompt submission. This covers both normal user turns and the
//     synthetic wake prompt written by Session.WakeStdin when the runner
//     detects new agentboard messages while the agent is idle.
//
// There is deliberately no SessionStart hook. The server seeds every task's
// id-directed inbound topic (`chat.<first-8-hex-of-task-id>`) when it issues
// the agentboard auth ticket for a runner assignment. That path is
// agent-runtime-neutral, covers no-worktree and non-Claude agents, and
// re-applies on resume or runner reassignment where the `(runner, task)` broker
// key changes. `harness.hello` remains opt-in discovery only.
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
// `permissions`) are preserved. Hooks whose command starts with
// `harness-cli ` but is not in the current harnessHookEntries list are
// pruned (see pruneStaleHarnessHooks) so retired hooks from older runner
// versions don't linger forever. The runner's hookSpec is then appended
// to `hooks.<event>` only if no entry with the same command is already
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

	pruneStaleHarnessHooks(root)
	mergeHarnessHooks(root)
	mergeHarnessAllow(root)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// pruneStaleHarnessHooks removes hooks whose command starts with
// harnessHookPrefix but is no longer listed in harnessHookEntries. This
// covers the case where an older runner installed a hook (e.g. a Stop
// hook) that the current code has retired: mergeHarnessHooks alone only
// adds, so without prune the legacy entry would persist for the lifetime
// of the worktree and keep firing. Empty hook groups and empty event
// arrays are deleted as a side effect so the resulting settings.json
// stays clean.
//
// Match policy: prefix-only (`harness-cli ...`). False positives (a user
// adding a `harness-cli` hook by hand for some reason) are tolerated
// because the runner is the only documented producer of such hooks in
// this single-user dogfood deployment; if that ever changes, switch to
// an explicit allow-list of retired commands and key on exact match.
func pruneStaleHarnessHooks(root map[string]any) {
	current := make(map[string]struct{}, len(harnessHookEntries))
	for _, e := range harnessHookEntries {
		current[e.Command] = struct{}{}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return
	}
	for event, raw := range hooks {
		groups, _ := raw.([]any)
		var keptGroups []any
		for _, g := range groups {
			group, _ := g.(map[string]any)
			hs, _ := group["hooks"].([]any)
			var keptHooks []any
			for _, h := range hs {
				hook, _ := h.(map[string]any)
				cmd, _ := hook["command"].(string)
				if strings.HasPrefix(cmd, harnessHookPrefix) {
					if _, ok := current[cmd]; !ok {
						continue // drop: harness-managed but no longer current
					}
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) == 0 {
				continue
			}
			group["hooks"] = keptHooks
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptGroups
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
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
