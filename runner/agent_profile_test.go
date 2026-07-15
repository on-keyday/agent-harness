package runner

import "testing"

func TestProfileSetResolve(t *testing.T) {
	def := AgentProfile{Name: "claude", Bin: "claude"}
	ps, err := NewProfileSet(def, []AgentProfile{{Name: "codex", Bin: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := ps.Resolve(""); p.Name != "claude" {
		t.Fatalf("empty→default got %q", p.Name)
	}
	if p, _ := ps.Resolve("codex"); p.Bin != "codex" {
		t.Fatalf("got %q", p.Bin)
	}
	if _, err := ps.Resolve("gemini"); err == nil {
		t.Fatal("unknown must error")
	}
}

func TestProfileSetDupName(t *testing.T) {
	_, err := NewProfileSet(AgentProfile{Name: "claude"}, []AgentProfile{{Name: "claude"}})
	if err == nil {
		t.Fatal("dup name must error")
	}
}

func TestParseAgentProfilesJSON(t *testing.T) {
	ps, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","oneshotArgv":["exec","{args}","{prompt}"],"resumeOneshotArgv":["exec","resume","--last","{args}","{prompt}"]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Name != "codex" {
		t.Fatalf("got %+v", ps)
	}
}
