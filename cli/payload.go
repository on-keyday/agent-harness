package cli

import "io"

// Where a publish's body came from. These strings are reported back to the
// caller (`source` on a send's ok line, the summary line on dispatch) because a
// byte count alone does not name the mistake that produces a wrong one: a
// caller reading `--data D|-` in the usage text as a positional runs
// `send --topic T -` and publishes the literal "-". That is one byte from
// `positional`, and only the second half distinguishes it from a genuine
// one-byte body.
const (
	PayloadSourceData       = "--data"
	PayloadSourcePositional = "positional"
	PayloadSourceStdin      = "stdin"
)

// ResolvePayload picks a publish body out of already-parsed values and names
// where it came from. Shared by `agent send`, `agent dispatch` and
// `board send`, so the verbs — identical body surfaces, one board — cannot
// disagree about what `--data`, a positional and a bare pipe each mean.
//
// dataSet says whether --data was typed: its default IS "-", so the value
// alone cannot tell `--data -` (asked for stdin) from nothing asked at all,
// which is what makes the positional fallback safe to apply only when no
// --data was given.
func ResolvePayload(dataSet bool, data, positional string, stdin io.Reader) ([]byte, string, error) {
	switch {
	case dataSet && data != "-":
		// explicit literal payload via --data
		return []byte(data), PayloadSourceData, nil
	case !dataSet && positional != "":
		// payload given as positional argument(s), joined ssh-style. This matches
		// the common `cmd <payload>` instinct so a forgotten --data doesn't
		// silently send an empty body (we used to ignore positionals entirely and
		// fall through to reading stdin).
		return []byte(positional), PayloadSourcePositional, nil
	default:
		// explicit `--data -`, or neither --data nor a positional given: read stdin.
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, "", err
		}
		return b, PayloadSourceStdin, nil
	}
}
