package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgentSettings_AgyMerge(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".agents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(sub, "hooks.json")
	existing := `{
		"lint-checker": {
			"PostToolUse": [
				{
					"matcher": "run_command",
					"hooks": [
						{"type": "command", "command": "./scripts/lint.sh"}
					]
				}
			]
		},
		"harness": {
			"Stop": [
				{"type": "command", "command": "harness-cli retired-stop"}
			]
		}
	}`
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

	// Idempotency: second write should produce identical output
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("repeat injection changed agy hooks: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(first, &root); err != nil {
		t.Fatal(err)
	}

	// User-defined hook "lint-checker" must be preserved
	lintChecker, ok := root["lint-checker"].(map[string]any)
	if !ok {
		t.Fatalf("user hook lint-checker lost: %v", root)
	}
	if len(lintChecker) == 0 {
		t.Fatalf("lint-checker empty: %v", lintChecker)
	}

	// Harness hook section
	harness, ok := root["harness"].(map[string]any)
	if !ok {
		t.Fatalf("harness section missing or not an object: %v", root)
	}

	// Stale hook "harness-cli retired-stop" must be pruned
	stopHandlers, ok := harness["Stop"].([]any)
	if !ok || len(stopHandlers) != 1 {
		t.Fatalf("Stop handlers unexpected: %v", harness["Stop"])
	}
	stopHook := stopHandlers[0].(map[string]any)
	if cmd := stopHook["command"].(string); cmd != "harness-cli lifecycle --hook agy idle" {
		t.Fatalf("Stop hook command = %q, want lifecycle --hook agy idle", cmd)
	}

	// Check Flat events: SessionStart, PreInvocation, Stop
	for _, ev := range []string{"SessionStart", "PreInvocation", "Stop"} {
		handlers, ok := harness[ev].([]any)
		if !ok || len(handlers) == 0 {
			t.Errorf("flat event %s missing or not an array", ev)
			continue
		}
		h, ok := handlers[0].(map[string]any)
		if !ok || h["type"] != "command" || !strings.HasPrefix(h["command"].(string), "harness-cli lifecycle --hook agy") {
			t.Errorf("flat event %s handler malformed: %v", ev, handlers[0])
		}
	}

	// Check Grouped events: PreToolUse, PostToolUse
	for _, ev := range []string{"PreToolUse", "PostToolUse"} {
		groups, ok := harness[ev].([]any)
		if !ok || len(groups) == 0 {
			t.Errorf("grouped event %s missing or not an array", ev)
			continue
		}
		g, ok := groups[0].(map[string]any)
		if !ok || g["matcher"] == nil || g["hooks"] == nil {
			t.Errorf("grouped event %s group malformed: %v", ev, groups[0])
		}
	}
}

func TestWriteAgentSettings_AgyMalformed(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ".agents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "hooks.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err == nil || !strings.Contains(err.Error(), "hooks.json") {
		t.Fatalf("wanted hooks.json malformed error, got %v", err)
	}
}
