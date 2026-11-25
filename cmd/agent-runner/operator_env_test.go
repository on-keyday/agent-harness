package main

import (
	"os"
	"testing"

	"github.com/on-keyday/agent-harness/connauth"
)

// A runner started with the operator secret in its environment must not carry
// it into anything it spawns; what agents DO need, like the server address,
// stays.
func TestScrubOperatorSecretDropsOnlyTheOperatorNames(t *testing.T) {
	t.Setenv(connauth.OperatorPSKEnv, "op")
	t.Setenv(connauth.OperatorPSKFileEnv, "/tmp/op")
	t.Setenv(connauth.CredentialEnv, "id:seed")
	t.Setenv(connauth.CredentialFileEnv, "/tmp/cred")
	t.Setenv("HARNESS_SERVER_CID", "ws:127.0.0.1:8539-*")
	scrubOperatorSecret()
	for _, k := range []string{connauth.OperatorPSKEnv, connauth.OperatorPSKFileEnv, connauth.CredentialEnv, connauth.CredentialFileEnv} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s survived the scrub with %q", k, v)
		}
	}
	if v := os.Getenv("HARNESS_SERVER_CID"); v != "ws:127.0.0.1:8539-*" {
		t.Errorf("HARNESS_SERVER_CID = %q, want it kept", v)
	}
}
