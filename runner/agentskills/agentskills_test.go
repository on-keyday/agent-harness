package agentskills

import "testing"

func TestSkillHarnessCLI(t *testing.T) {
	b, err := Skill("harness-cli")
	if err != nil {
		t.Fatalf("Skill(harness-cli): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("harness-cli skill is empty")
	}
}

func TestSkillUnknown(t *testing.T) {
	if _, err := Skill("nope"); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
