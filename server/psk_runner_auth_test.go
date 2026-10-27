package server

import (
	"crypto/ed25519"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func buildRunnerAuthReq(priv ed25519.PrivateKey, transcript []byte, rh protocol.RunnerHello) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Runner}
	req.SetBinder(nil)
	auth := protocol.RunnerAuth{Hello: rh}
	copy(auth.Key[:], priv.Public().(ed25519.PublicKey))
	copy(auth.Sig[:], cli.SignRunnerAuth(priv, transcript))
	req.SetRunnerAuth(auth)
	return req
}

func testRunnerHello(host string) protocol.RunnerHello {
	rh := protocol.RunnerHello{Version: 1, MaxTasks: 2, RunnerId: testRunnerID(host)}
	rh.SetHostname([]byte(host))
	return rh
}

// enrolledTestRunner is a runner key and an enrollment store holding it, for
// tests that run a real server and handshake as a runner.
func enrolledTestRunner(t *testing.T) (ed25519.PrivateKey, *RunnerKeyStore) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	st, err := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: pub, Label: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	return priv, st
}

func gateWithKeys(t *testing.T, keys ...ed25519.PublicKey) *pskGate {
	t.Helper()
	var recs []RunnerKeyRecord
	for _, k := range keys {
		recs = append(recs, RunnerKeyRecord{Key: k, Label: "test"})
	}
	st, err := LoadRunnerKeyStore("", recs)
	if err != nil {
		t.Fatal(err)
	}
	g := newPSKGate()
	g.runnerKeys = st
	return g
}

// The negative control: before this change a runner proved nothing, so any
// host that reached the server registered.
func TestGateRunnerUnknownKeyIsNotEnrolled(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	g := gateWithKeys(t)
	var sent []byte
	_, shouldClose, accepted := g.Check(buildPskAuthData(t, buildRunnerAuthReq(priv, testTranscript, testRunnerHello("h"))), testTranscript, func(b []byte) { sent = append(sent, b...) })
	if !shouldClose || accepted != nil || g.Authed() {
		t.Fatalf("unknown key accepted: shouldClose=%v accepted=%v authed=%v", shouldClose, accepted != nil, g.Authed())
	}
	if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_NotEnrolled {
		t.Fatalf("status %v, want not_enrolled", st)
	}
}

func TestGateRunnerSignatureOverAnotherTranscriptIsRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	g := gateWithKeys(t, pub)
	var sent []byte
	req := buildRunnerAuthReq(priv, []byte("another connection"), testRunnerHello("h"))
	_, shouldClose, _ := g.Check(buildPskAuthData(t, req), testTranscript, func(b []byte) { sent = append(sent, b...) })
	if !shouldClose || g.Authed() {
		t.Fatal("a signature over another transcript was accepted")
	}
	if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_BadPsk {
		t.Fatalf("status %v, want bad_psk", st)
	}
}

func TestGateRunnerEnrolledKeyIsAccepted(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	g := gateWithKeys(t, pub)
	var sent []byte
	_, shouldClose, accepted := g.Check(buildPskAuthData(t, buildRunnerAuthReq(priv, testTranscript, testRunnerHello("h"))), testTranscript, func(b []byte) { sent = append(sent, b...) })
	if shouldClose || accepted == nil || !g.Authed() {
		t.Fatalf("enrolled key refused: status %v", parsePskResponse(t, sent))
	}
	if g.Role() != protocol.AuthRole_Runner || !g.RunnerKey().Equal(pub) {
		t.Fatalf("role=%v key=%x", g.Role(), g.RunnerKey())
	}
}
