package agent

import (
	"flag"
	"io"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
)

// The body-source names, as cli.ResolvePayload reports them.
const (
	sourceData       = cli.PayloadSourceData
	sourcePositional = cli.PayloadSourcePositional
	sourceStdin      = cli.PayloadSourceStdin
)

// resolvePayload is cli.ResolvePayload for a caller holding a parsed FlagSet.
//
// fs must already be Parsed: this reads fs.Args() for the positional form and
// fs.Visit to tell `--data -` (asked for stdin) from the `-` default (nothing
// asked at all).
func resolvePayload(fs *flag.FlagSet, data string, stdin io.Reader) ([]byte, string, error) {
	dataSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			dataSet = true
		}
	})
	return cli.ResolvePayload(dataSet, data, strings.Join(fs.Args(), " "), stdin)
}

// resolvePayloadFrom is cli.ResolvePayload under the name this package's
// callers already use.
func resolvePayloadFrom(dataSet bool, data, positional string, stdin io.Reader) ([]byte, string, error) {
	return cli.ResolvePayload(dataSet, data, positional, stdin)
}
