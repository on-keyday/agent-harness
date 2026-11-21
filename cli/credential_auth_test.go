package cli

import (
	"crypto/ed25519"
	"testing"
)

func TestCredentialAuthSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []byte("transcript of connection A")
	sig := SignCredentialAuth(priv, transcript)
	if !VerifyCredentialAuth(pub, transcript, sig) {
		t.Fatal("a signature over the same transcript does not verify")
	}
	flipped := append([]byte(nil), transcript...)
	flipped[0] ^= 1
	if VerifyCredentialAuth(pub, flipped, sig) {
		t.Fatal("a signature verified over a transcript with one byte flipped")
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if VerifyCredentialAuth(otherPub, transcript, sig) {
		t.Fatal("a signature verified against another key")
	}
	if VerifyCredentialAuth(pub[:16], transcript, sig) {
		t.Fatal("a short public key verified")
	}
}

// One key, two uses: a runner-auth signature must not pass as credential-auth
// over the same transcript, nor the other way round.
func TestCredentialAuthIsNotRunnerAuth(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	transcript := []byte("transcript")
	if VerifyCredentialAuth(pub, transcript, SignRunnerAuth(priv, transcript)) {
		t.Fatal("a runner-auth signature verified as credential-auth")
	}
	if VerifyRunnerAuth(pub, transcript, SignCredentialAuth(priv, transcript)) {
		t.Fatal("a credential-auth signature verified as runner-auth")
	}
}
