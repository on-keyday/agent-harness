//go:build js

package connauth

// ActiveCredential in the browser is none: the WebUI is an operator surface
// and never loads a credential.
func ActiveCredential() (*Credential, error) { return nil, nil }
