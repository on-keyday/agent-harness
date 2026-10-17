package protocol

import "testing"

func candidateFixture(host string, id byte, profiles ...string) RunnerCandidate {
	var c RunnerCandidate
	c.RunnerId.Id[0] = id
	c.SetCid([]byte("ws:192.168.3.14:52826-39936"))
	c.SetHostname([]byte(host))
	c.SetMatchedRoot([]byte("/home/kforfk/workspace/sisaku-security/rule-review"))
	c.ActiveTasks, c.MaxTasks = 2, 8
	names := make([]AgentProfileName, len(profiles))
	for i, p := range profiles {
		names[i].SetName([]byte(p))
	}
	c.SetProfiles(names)
	return c
}

var elevenProfiles = []string{"claude", "codex", "agy", "opencode", "pi", "bash",
	"sandbox-claude", "sandbox-codex", "sandbox-agy", "sandbox-bash", "sandbox-opencode"}

func TestRunnerCandidatesBodyRoundTrip(t *testing.T) {
	var body RunnerCandidatesBody
	body.SetCandidates([]RunnerCandidate{
		candidateFixture("gmkhost", 1, elevenProfiles...),
		candidateFixture("gmkhost", 2, elevenProfiles...),
	})
	raw, err := body.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RunnerCandidatesBody
	if err := got.DecodeExact(raw); err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 2 || len(got.Candidates[1].Profiles) != 11 ||
		string(got.Candidates[1].Profiles[10].Name) != "sandbox-opencode" || got.Candidates[1].RunnerId.Id[0] != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

// The ambiguous answer is fixed-size: whatever the candidate count, the
// response itself must fit one datagram (~1170 B).
func TestAmbiguousResponsesAreFixedSize(t *testing.T) {
	var oi OpenInteractiveResponse
	oi.Status = OpenInteractiveStatus_AmbiguousRunner
	if !oi.SetCandidatesStreamId(7) {
		t.Fatal("SetCandidatesStreamId on OpenInteractiveResponse")
	}
	var sub SubmitResponse
	sub.Status = SubmitStatus_AmbiguousRunner
	if !sub.SetCandidatesStreamId(7) {
		t.Fatal("SetCandidatesStreamId on SubmitResponse")
	}
	for name, enc := range map[string]interface{ EncodeCopy([]byte) ([]byte, error) }{"open": &oi, "submit": &sub} {
		raw, err := enc.EncodeCopy(nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(raw) > 64 {
			t.Fatalf("%s ambiguous response is %d bytes; want fixed and small", name, len(raw))
		}
	}
	var back SubmitResponse
	raw, _ := sub.EncodeCopy(nil)
	if _, err := back.Decode(raw); err != nil || back.CandidatesStreamId() == nil || *back.CandidatesStreamId() != 7 {
		t.Fatalf("submit stream id did not round-trip: %v %+v", err, back)
	}
}
