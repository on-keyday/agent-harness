//go:build !js

package main

import (
	"io"
	"os"
	"strings"
)

// What the forward family's bodies need beyond the client: the --http-body
// resolver. (The tap render mode is cli.TapModeByName, shared with the TUI.)

// readFlagBody resolves a --http-body value: a literal, @file, or - for stdin.
func readFlagBody(v string) ([]byte, error) {
	switch {
	case v == "":
		return nil, nil
	case v == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(v, "@"):
		return os.ReadFile(v[1:])
	default:
		return []byte(v), nil
	}
}
