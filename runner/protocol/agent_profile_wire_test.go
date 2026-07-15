package protocol

import "testing"

// TestRunnerHelloAgentProfilesRoundTrip verifies RunnerHello.AgentProfiles
// (a list of the new AgentProfileName format) survives a round-trip.
//
// RunnerHello has no top-level Decode([]byte) — like ClientHello, it is only
// ever decoded as an embedded field of PskAuthRequest (see
// psk_handshake_test.go's TestPskAuthRequestRunnerRoleRoundTrip, and
// server/psk.go / runner/connect.go, which never call RunnerHello.Decode or
// .Read standalone). Mirror that same wrapping pattern here instead of
// exercising the unused low-level RunnerHello.Read(io.Reader) API directly.
func TestRunnerHelloAgentProfilesRoundTrip(t *testing.T) {
	var p1, p2 AgentProfileName
	if !p1.SetName([]byte("claude")) {
		t.Fatal("p1.SetName failed")
	}
	if !p2.SetName([]byte("codex")) {
		t.Fatal("p2.SetName failed")
	}

	var rh RunnerHello
	rh.Version = 1
	rh.SetHostname([]byte("runner-host"))
	if !rh.SetAgentProfiles([]AgentProfileName{p1, p2}) {
		t.Fatal("SetAgentProfiles failed")
	}

	var orig PskAuthRequest
	orig.Role = AuthRole_Runner
	if !orig.SetBinder(nil) {
		t.Fatal("SetBinder(nil) returned false")
	}
	if !orig.SetRunnerHello(rh) {
		t.Fatal("SetRunnerHello failed")
	}

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got PskAuthRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	gotRH := got.RunnerHello()
	if gotRH == nil {
		t.Fatal("RunnerHello() returned nil")
	}
	if len(gotRH.AgentProfiles) != 2 || string(gotRH.AgentProfiles[1].Name) != "codex" {
		t.Fatalf("profiles round-trip: got %+v", gotRH.AgentProfiles)
	}
	if string(gotRH.AgentProfiles[0].Name) != "claude" {
		t.Fatalf("profiles[0] round-trip: got %+v", gotRH.AgentProfiles)
	}
}

// TestSubmitRequestAgentProfileRoundTrip verifies SubmitRequest.AgentProfile
// (the client-requested agent profile name) survives a round-trip.
func TestSubmitRequestAgentProfileRoundTrip(t *testing.T) {
	var s SubmitRequest
	s.SetRepoPath([]byte("/r"))
	s.Selector = RunnerSelector{Kind: RunnerSelectorKind_Any}
	s.SetPrompt([]byte("hi"))
	if !s.SetAgentProfile([]byte("codex")) {
		t.Fatal("SetAgentProfile failed")
	}

	buf, err := s.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got SubmitRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(got.AgentProfile) != "codex" {
		t.Fatalf("AgentProfile: got %q, want %q", got.AgentProfile, "codex")
	}
}

// TestRunnerCandidateProfileRoundTrip verifies RunnerCandidate.Profile (the
// new u16-length-prefixed field) survives a round-trip inside
// OpenInteractiveResponse's ambiguous_runner conditional block, mirroring
// candidate_roundtrip_test.go's existing coverage of the sibling fields.
func TestRunnerCandidateProfileRoundTrip(t *testing.T) {
	var c RunnerCandidate
	c.SetCid([]byte("ws:192.168.3.14:34184-30218"))
	c.SetHostname([]byte("gmkhost-codex"))
	if !c.SetProfile([]byte("codex")) {
		t.Fatal("SetProfile failed")
	}

	resp := OpenInteractiveResponse{Status: OpenInteractiveStatus_AmbiguousRunner}
	if !resp.SetCandidates([]RunnerCandidate{c}) {
		t.Fatalf("SetCandidates failed")
	}

	raw, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got OpenInteractiveResponse
	if _, err := got.Decode(raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotCandidates := got.Candidates()
	if gotCandidates == nil || len(*gotCandidates) != 1 {
		t.Fatalf("candidates=%v", gotCandidates)
	}
	if string((*gotCandidates)[0].Profile) != "codex" {
		t.Fatalf("candidate profile mismatch: %+v", (*gotCandidates)[0])
	}
}

// TestSubmitStatusProfileUnavailableRoundTrip verifies the new
// SubmitStatus_ProfileUnavailable enum value round-trips like its siblings
// (see psk_handshake_test.go's TestPskAuthResponseRoundTrip for the same
// per-enum-value pattern).
func TestSubmitStatusProfileUnavailableRoundTrip(t *testing.T) {
	orig := SubmitResponse{Status: SubmitStatus_ProfileUnavailable}
	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got SubmitResponse
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Status != SubmitStatus_ProfileUnavailable {
		t.Errorf("Status: got %v, want SubmitStatus_ProfileUnavailable", got.Status)
	}
}
