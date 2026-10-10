//go:build js

package cli

import "crypto/ed25519"

// GetServerPubKey is nil in the browser: the page and its wasm are served by
// the server being authenticated, so an impostor serves a page without the
// check (spec 2026-10-11, "Why the WebUI is out of scope").
func GetServerPubKey() (ed25519.PublicKey, error) { return nil, nil }
