package agentskills

import (
	"testing"
)

func TestDescription(t *testing.T) {
	// Every listed skill must expose a non-empty frontmatter description.
	names, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	for _, n := range names {
		d, err := Description(n)
		if err != nil {
			t.Errorf("Description(%q): %v", n, err)
			continue
		}
		if d == "" {
			t.Errorf("skill %q has an empty description", n)
		}
	}
	// A skill without frontmatter yields "" (not an error path we can hit via
	// real skills, so exercise the parser directly).
	if got := frontmatterField([]byte("# no frontmatter\n"), "description"); got != "" {
		t.Errorf("frontmatterField(no frontmatter) = %q, want empty", got)
	}
	if got := frontmatterField([]byte("---\ndescription: hi there\n---\n"), "description"); got != "hi there" {
		t.Errorf("frontmatterField = %q, want %q", got, "hi there")
	}
}

func TestList(t *testing.T) {
	names, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	// Every listed name must resolve to a non-empty skill, and the list must
	// stay sorted. Names are enumerated from the embed FS, so this also guards
	// against a directory sneaking in without a SKILL.md.
	if len(names) == 0 {
		t.Fatal("List() returned no skills")
	}
	for i, n := range names {
		if i > 0 && names[i-1] > n {
			t.Errorf("List() not sorted: %q before %q", names[i-1], n)
		}
		b, err := Skill(n)
		if err != nil || len(b) == 0 {
			t.Errorf("listed skill %q does not resolve: err=%v len=%d", n, err, len(b))
		}
	}
	// The core skills must be present.
	want := map[string]bool{"harness-cli": false, "independent-review": false, "landing-to-main": false, "session-debugging": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("List() missing expected skill %q", n)
		}
	}
}

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

func TestSkillSessionDebugging(t *testing.T) {
	b, err := Skill("session-debugging")
	if err != nil {
		t.Fatalf("Skill(session-debugging): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("session-debugging skill is empty")
	}
}

func TestSkillUnknown(t *testing.T) {
	if _, err := Skill("nope"); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
