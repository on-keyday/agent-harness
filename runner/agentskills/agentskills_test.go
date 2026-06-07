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

func TestSkillIndependentReview(t *testing.T) {
	b, err := Skill("independent-review")
	if err != nil {
		t.Fatalf("Skill(independent-review): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("independent-review skill is empty")
	}
}

func TestSkillLandingToMain(t *testing.T) {
	b, err := Skill("landing-to-main")
	if err != nil {
		t.Fatalf("Skill(landing-to-main): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("landing-to-main skill is empty")
	}
}

func TestSkillUnknown(t *testing.T) {
	if _, err := Skill("nope"); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
