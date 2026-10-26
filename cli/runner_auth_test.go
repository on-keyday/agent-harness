package cli

import (
	"crypto/ed25519"
	"testing"
)

func TestRunnerAuthSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []byte("transcript of connection A")
	sig := SignRunnerAuth(priv, transcript)
	if !VerifyRunnerAuth(pub, transcript, sig) {
		t.Fatal("a signature over the same transcript does not verify")
	}
	if VerifyRunnerAuth(pub, []byte("transcript of connection B"), sig) {
		t.Fatal("a signature verified over another connection's transcript")
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if VerifyRunnerAuth(otherPub, transcript, sig) {
		t.Fatal("a signature verified against another key")
	}
	// The server-auth context must not be accepted as runner auth: a signature
	// is valid for one use of the key only.
	serverSig := ed25519.Sign(priv, append([]byte(serverAuthContext), transcript...))
	if VerifyRunnerAuth(pub, transcript, serverSig) {
		t.Fatal("a signature made for another purpose verified as runner auth")
	}
}
