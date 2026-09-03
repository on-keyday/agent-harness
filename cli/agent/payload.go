package agent

import (
	"flag"
	"io"
	"strings"
)

// Where a publish's body came from. These strings are reported back to the
// caller (`source` on send's ok line, the summary line on dispatch) because a
// byte count alone does not name the mistake that produces a wrong one: an
// agent reading `--data D|-` in the usage text as a positional runs
// `agent send --topic T -` and publishes the literal "-". That is one byte
// from `positional`, and only the second half distinguishes it from a genuine
// one-byte body.
const (
	sourceData       = "--data"
	sourcePositional = "positional"
	sourceStdin      = "stdin"
)

// resolvePayload picks the publish body out of the already-parsed flag set and
// names where it came from. Shared by `agent send` and `agent dispatch` so the
// two verbs — identical flag surfaces, one board — cannot disagree about what
// `--data`, a positional and a bare pipe each mean.
//
// fs must already be Parsed: this reads fs.Args() for the positional form and
// fs.Visit to tell `--data -` (asked for stdin) from the `-` default (nothing
// asked at all), which is what makes the positional fallback safe to apply
// only when no --data was given.
func resolvePayload(fs *flag.FlagSet, data string, stdin io.Reader) ([]byte, string, error) {
	dataSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			dataSet = true
		}
	})
	return resolvePayloadFrom(dataSet, data, strings.Join(fs.Args(), " "), stdin)
}

// resolvePayloadFrom is the same decision made from already-parsed values, for
// callers that got theirs from the declaration rather than a FlagSet.
func resolvePayloadFrom(dataSet bool, data, positional string, stdin io.Reader) ([]byte, string, error) {
	switch {
	case dataSet && data != "-":
		// explicit literal payload via --data
		return []byte(data), sourceData, nil
	case !dataSet && positional != "":
		// payload given as positional argument(s), joined ssh-style. This matches
		// the common `cmd <payload>` instinct so a forgotten --data doesn't
		// silently send an empty body (we used to ignore positionals entirely and
		// fall through to reading stdin).
		return []byte(positional), sourcePositional, nil
	default:
		// explicit `--data -`, or neither --data nor a positional given: read stdin.
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, "", err
		}
		return b, sourceStdin, nil
	}
}
