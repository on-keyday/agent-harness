//go:build js

package cli

// activeCredential in the browser is none: the WebUI is an operator surface
// and never loads a credential.
func activeCredential() (*Credential, error) { return nil, nil }
