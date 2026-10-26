package integration

import (
	"crypto/ed25519"

	"github.com/on-keyday/agent-harness/server"
)

// testRunnerKey is the key every runner in this package signs with, and
// testRunnerKeys() a store that has it enrolled, so the tests that are not
// about enrollment see a fleet that has already migrated.
var testRunnerKey = func() ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	return k
}()

func testRunnerKeys() *server.RunnerKeyStore {
	st, err := server.LoadRunnerKeyStore("", []server.RunnerKeyRecord{
		{Key: testRunnerKey.Public().(ed25519.PublicKey), Label: "integration", EnrolledBy: "operator"},
	})
	if err != nil {
		panic(err)
	}
	return st
}
