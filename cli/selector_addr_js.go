//go:build js

package cli

import "github.com/on-keyday/objtrsf/objproto"

// addrParseOptions omits ResolveAddr: it falls back to net.LookupIP for a
// hostname-shaped addr, and DNS is not available under GOOS=js.
//
// Not a functional gap in practice — the WebUI runner picker passes back a
// literal cid the server gave it, already an ip:port. An operator who types a
// hostname into the command input's --runner gets a parse error instead of a
// silent lookup, which is the honest outcome for a build that cannot resolve.
func addrParseOptions() objproto.ParseOption {
	return objproto.ParseOption_AllowRandomID
}
