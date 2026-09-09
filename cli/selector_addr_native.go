//go:build !js

package cli

import "github.com/on-keyday/objtrsf/objproto"

// addrParseOptions are the options for parsing an operator-supplied connection
// id. ResolveAddr lets a hostname-shaped addr be typed instead of an IP; it
// reaches net.LookupIP, which is the one thing in runner selection that needs
// the OS network stack — hence the split from the js variant.
func addrParseOptions() objproto.ParseOption {
	return objproto.ParseOption_AllowRandomID | objproto.ParseOption_ResolveAddr
}
