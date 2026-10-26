package cli

import "crypto/ed25519"

// RunnerKeyFileEnv names the runner's key file when --runner-key-file is not
// given. The runner unsets it before spawning anything, like the operator PSK
// names: agents have no use for the path.
const RunnerKeyFileEnv = "HARNESS_RUNNER_KEY_FILE"

// runnerAuthContext keeps a runner's signature from being valid for any other
// use of the key, as serverAuthContext does for the server's.
const runnerAuthContext = "agent-harness runner-auth v1\x00"

func runnerAuthMessage(transcript []byte) []byte {
	msg := make([]byte, 0, len(runnerAuthContext)+len(transcript))
	msg = append(msg, runnerAuthContext...)
	return append(msg, transcript...)
}

// SignRunnerAuth is RunnerAuth.sig: the runner proves its enrolled key over
// this connection's objproto transcript, so the proof is worthless on any
// other connection.
func SignRunnerAuth(key ed25519.PrivateKey, transcript []byte) []byte {
	return ed25519.Sign(key, runnerAuthMessage(transcript))
}

// VerifyRunnerAuth is the server's check of RunnerAuth.sig.
func VerifyRunnerAuth(pub ed25519.PublicKey, transcript, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, runnerAuthMessage(transcript), sig)
}
