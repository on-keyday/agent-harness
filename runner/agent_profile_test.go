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

func TestParseAgentProfilesJSONCarriesLogFormat(t *testing.T) {
	got, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","logFormat":"codex-jsonl"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].LogFormat != "codex-jsonl" {
		t.Fatalf("got %+v, want one profile with LogFormat codex-jsonl", got)
	}
}

func TestNewProfileSetAcceptsUnknownLogFormat(t *testing.T) {
	// A misconfigured profile must not stop the runner from starting; the
	// decoder resolves an unrecognised name to passthrough.
	_, err := NewProfileSet(AgentProfile{Name: "claude", Bin: "claude", LogFormat: "nonsense"}, nil)
	if err != nil {
		t.Fatalf("NewProfileSet rejected an unknown log format: %v", err)
	}
}

func TestUnrecognisedLogFormats(t *testing.T) {
	ps, err := NewProfileSet(
		AgentProfile{Name: "claude", Bin: "claude", LogFormat: "claude-stream-json"},
		[]AgentProfile{
			{Name: "codex", Bin: "codex", LogFormat: "codex-jsonl"},
			{Name: "bash", Bin: "bash"},
			{Name: "weird", Bin: "weird", LogFormat: "nonsense"},
		},
	)
	if err != nil {
		t.Fatalf("NewProfileSet: %v", err)
	}
	got := ps.UnrecognisedLogFormats()
	if len(got) != 1 || got[0] != `weird: "nonsense"` {
		t.Fatalf("got %v, want exactly the weird profile", got)
	}
}
