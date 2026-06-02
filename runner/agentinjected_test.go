package runner

import "testing"

func TestHarnessInjectedPathsCoverNewInjections(t *testing.T) {
	has := func(p string) bool {
		for _, x := range HarnessInjectedPaths {
			if x == p {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"AGENTS.md", "GEMINI.md", ".agents/skills/"} {
		if !has(want) {
			t.Errorf("HarnessInjectedPaths missing %q (writer/list out of sync)", want)
		}
	}
}
