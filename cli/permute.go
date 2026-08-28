package cli

import "flag"

// ParsePermuted parses fs but tolerates flags appearing after positional args.
// Go's stdlib flag stops at the first non-flag token, so `cmd <id> --flag` would
// otherwise silently drop --flag (it lands in fs.Args() and is ignored). We peel
// positionals one at a time and re-parse the remainder, making flag position
// irrelevant — the model can write the flag before or after the id and it works.
//
// Use this ONLY for commands whose positionals can never begin with '-' (e.g. a
// hex task id, an agentboard topic name). For free-form text positionals, keep
// flags strictly before the positional instead: a '-'-leading word is
// indistinguishable from a flag, and a '--' terminator would not survive the
// peel loop.
//
// Reach for it wherever a verb's own usage line puts a flag AFTER a positional,
// which is where the silent drop actually bites: `board purge <topic> --seq N`
// is what the help text told operators to type, and stdlib parsing left --seq
// unread, so the call fell through to the whole-topic form and destroyed the
// ring it was asked to take one message from. A flag that is ignored is bad; a
// flag whose absence WIDENS the operation is how a typo becomes data loss.
func ParsePermuted(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
	return positionals, nil
}
