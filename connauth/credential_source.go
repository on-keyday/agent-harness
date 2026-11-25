//go:build !js

package connauth

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// credentialFilePath is --credential-file, installed once by harness-cli's
// flag parsing (SetCredentialFile). A package variable, not a Dial argument:
// every verb dials, and the credential is a property of the process, as the
// operator PSK in its environment is.
var credentialFilePath string

// SetCredentialFile records --credential-file for this process.
func SetCredentialFile(path string) { credentialFilePath = path }

// LoadCredential finds the credential this process carries, first match wins:
// flagPath (--credential-file), HARNESS_CREDENTIAL_FILE, HARNESS_CREDENTIAL.
// nil, nil when there is none. A source that is named but unusable is an
// error, never a quiet fall through to the next one: a script that named its
// credential file must not end up as something else.
func LoadCredential(flagPath string) (*Credential, error) {
	switch {
	case flagPath != "":
		return loadCredentialFile(flagPath, "--credential-file")
	case os.Getenv(CredentialFileEnv) != "":
		return loadCredentialFile(os.Getenv(CredentialFileEnv), CredentialFileEnv)
	case os.Getenv(CredentialEnv) != "":
		c, err := ParseCredential(os.Getenv(CredentialEnv))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", CredentialEnv, err)
		}
		return &c, nil
	}
	return nil, nil
}

// ActiveCredential is LoadCredential with this process's --credential-file.
func ActiveCredential() (*Credential, error) { return LoadCredential(credentialFilePath) }

// loadCredentialFile reads a credential file. The mode is read from the open
// descriptor, so the file checked is the file read. A file group- or
// world-readable is refused: the seed in it is the whole credential. Windows
// has no mode bits to read, so there it is not checked.
func loadCredentialFile(path, source string) (*Credential, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: credential file %s has mode %04o, readable by group or other: chmod 600 it", source, path, fi.Mode().Perm())
	}
	if fi.Size() > 4096 {
		return nil, fmt.Errorf("%s: %s is not a credential file (%d bytes)", source, path, fi.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	c, err := ParseCredential(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", source, path, err)
	}
	return &c, nil
}
