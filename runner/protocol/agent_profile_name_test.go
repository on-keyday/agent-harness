package protocol

import "testing"

func TestNormalizeAgentProfileName(t *testing.T) {
	cases := map[string]string{
		"claude":      "claude",
		"claude.exe":  "claude",
		"Claude.EXE":  "Claude",
		"codex.cmd":   "codex",
		"run.bat":     "run",
		"legacy.com":  "legacy",
		"bash":        "bash",
		"a.exe.exe":   "a.exe", // strips exactly one suffix
		".exe":        ".exe",  // never normalize to empty
		"":            "",
		"claude.json": "claude.json", // not an executable extension
	}
	for in, want := range cases {
		if got := NormalizeAgentProfileName(in); got != want {
			t.Errorf("NormalizeAgentProfileName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEqualAgentProfileName(t *testing.T) {
	if !EqualAgentProfileName("claude.exe", "claude") {
		t.Error("claude.exe must equal claude")
	}
	if !EqualAgentProfileName("claude", "claude") {
		t.Error("identity must hold")
	}
	if EqualAgentProfileName("claude", "codex") {
		t.Error("distinct names must not match")
	}
	if EqualAgentProfileName("Claude", "claude") {
		t.Error("base name comparison stays case-sensitive; only the extension is case-insensitive")
	}
}
