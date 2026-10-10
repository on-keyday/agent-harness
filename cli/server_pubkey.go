//go:build !js

package cli

import (
	"crypto/ed25519"
	"os"
)

// GetServerPubKey reads the server public key this process verifies against
// (HARNESS_SERVER_PUBKEY, then HARNESS_SERVER_PUBKEY_FILE). Neither set →
// nil, nil. Set but unusable → error: dialing without the check the operator
// asked for would be a silent downgrade.
func GetServerPubKey() (ed25519.PublicKey, error) {
	return ResolveServerPubKey(os.Getenv(ServerPubKeyEnv), os.Getenv(ServerPubKeyFileEnv))
}
