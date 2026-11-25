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
	if !orig.SetRunnerAuth(RunnerAuth{Hello: rh}) {
		t.Fatal("SetRunnerAuth failed")
	}

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got PskAuthRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	ga := got.RunnerAuth()
	if ga == nil {
		t.Fatal("RunnerAuth() returned nil")
	}
	gotRH := &ga.Hello
	if len(gotRH.AgentProfiles) != 2 || string(gotRH.AgentProfiles[1].Name) != "codex" {
		t.Fatalf("profiles round-trip: got %+v", gotRH.AgentProfiles)
	}
	if string(gotRH.AgentProfiles[0].Name) != "claude" {
		t.Fatalf("profiles[0] round-trip: got %+v", gotRH.AgentProfiles)
	}
}

// TestOpenInteractiveRequestAgentProfileRoundTrip verifies
// OpenInteractiveRequest.AgentProfile
// (the client-requested agent profile name) survives a round-trip.
func TestOpenInteractiveRequestAgentProfileRoundTrip(t *testing.T) {
	var s OpenInteractiveRequest
	s.SetRepoPath([]byte("/r"))
	s.Selector = RunnerSelector{Kind: RunnerSelectorKind_Any}
	if !s.SetAgentProfile([]byte("codex")) {
		t.Fatal("SetAgentProfile failed")
	}

	buf, err := s.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got OpenInteractiveRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(got.AgentProfile) != "codex" {
		t.Fatalf("AgentProfile: got %q, want %q", got.AgentProfile, "codex")
	}
}

// TestOpenInteractiveStatusProfileUnavailableRoundTrip verifies the
// OpenInteractiveStatus_ProfileUnavailable enum value round-trips like its
// siblings (see psk_handshake_test.go's TestPskAuthResponseRoundTrip for the
// same per-enum-value pattern).
func TestOpenInteractiveStatusProfileUnavailableRoundTrip(t *testing.T) {
	orig := OpenInteractiveResponse{Status: OpenInteractiveStatus_ProfileUnavailable}
	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got OpenInteractiveResponse
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Status != OpenInteractiveStatus_ProfileUnavailable {
		t.Errorf("Status: got %v, want OpenInteractiveStatus_ProfileUnavailable", got.Status)
	}
}
