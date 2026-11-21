package cli

import "crypto/ed25519"

// credentialAuthContext keeps a credential's signature from being valid for
// any other use of the key — a runner key signing the same transcript does not
// produce a credential proof (credential-tasks D4).
const credentialAuthContext = "agent-harness credential-auth v1\x00"

func credentialAuthMessage(transcript []byte) []byte {
	msg := make([]byte, 0, len(credentialAuthContext)+len(transcript))
	msg = append(msg, credentialAuthContext...)
	return append(msg, transcript...)
}

// SignCredentialAuth is CredentialAuth.sig: the credential's holder proves its
// key over this connection's objproto transcript, so the proof is worthless
// on any other connection.
func SignCredentialAuth(key ed25519.PrivateKey, transcript []byte) []byte {
	return ed25519.Sign(key, credentialAuthMessage(transcript))
}

// VerifyCredentialAuth is the server's check of CredentialAuth.sig against
// the public key stored on the credential task.
func VerifyCredentialAuth(pub ed25519.PublicKey, transcript, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, credentialAuthMessage(transcript), sig)
}
