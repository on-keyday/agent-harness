package main

import (
	"os"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// A runner started with the operator secret in its environment must not carry
// it into anything it spawns; the connect PSK, which agents DO need, stays.
func TestScrubOperatorSecretDropsOnlyTheOperatorNames(t *testing.T) {
	t.Setenv(cli.OperatorPSKEnv, "op")
	t.Setenv(cli.OperatorPSKFileEnv, "/tmp/op")
	t.Setenv("HARNESS_PSK", "connect")
	scrubOperatorSecret()
	for _, k := range []string{cli.OperatorPSKEnv, cli.OperatorPSKFileEnv} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s survived the scrub with %q", k, v)
		}
	}
	if v := os.Getenv("HARNESS_PSK"); v != "connect" {
		t.Errorf("HARNESS_PSK = %q, want the connect PSK kept", v)
	}
}
