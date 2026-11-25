//go:build js

package cli

import (
	"net/url"
	"strings"
	"syscall/js"
)

// GetOperatorPSK reads the operator secret from the URL fragment
// (#psk=<value>; the fragment keeps its name so existing links work).
// Returns nil when the fragment is absent or contains no psk= key.
func GetOperatorPSK() []byte {
	hash := js.Global().Get("location").Get("hash").String()
	hash = strings.TrimPrefix(hash, "#")
	vals, err := url.ParseQuery(hash)
	if err != nil {
		return nil
	}
	v := vals.Get("psk")
	if v == "" {
		return nil
	}
	return []byte(v)
}

// resolveBinderPSK in the browser is always the operator surface: WASM never
// runs as an in-task agent. The #psk fragment IS the operator secret.
func resolveBinderPSK() []byte {
	return GetOperatorPSK()
}
