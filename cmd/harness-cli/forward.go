//go:build !js

package main

import (
	"io"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
)

// What the forward family's bodies need beyond the client: the --http-body
// resolver and the tap render mode.

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

// tapModeByName maps the declaration's mode word onto the renderer. The
// mutual exclusion between the four is enforced in the verb's Build, so by the
// time this runs exactly one was chosen.
func tapModeByName(name string) cli.TapRenderMode {
	switch name {
	case "text":
		return cli.TapText
	case "raw":
		return cli.TapRaw
	case "json":
		return cli.TapJSON
	default:
		return cli.TapHex
	}
}
