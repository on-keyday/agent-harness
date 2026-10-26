package main

import (
	"os"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// A runner started with the operator secret in its environment must not carry
// it into anything it spawns; what agents DO need, like the server address,
// stays.
func TestScrubOperatorSecretDropsOnlyTheOperatorNames(t *testing.T) {
	t.Setenv(cli.OperatorPSKEnv, "op")
	t.Setenv(cli.OperatorPSKFileEnv, "/tmp/op")
	t.Setenv("HARNESS_SERVER_CID", "ws:127.0.0.1:8539-*")
	scrubOperatorSecret()
	for _, k := range []string{cli.OperatorPSKEnv, cli.OperatorPSKFileEnv} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s survived the scrub with %q", k, v)
		}
	}
	if v := os.Getenv("HARNESS_SERVER_CID"); v != "ws:127.0.0.1:8539-*" {
		t.Errorf("HARNESS_SERVER_CID = %q, want it kept", v)
	}
}
