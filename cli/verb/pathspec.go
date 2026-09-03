package verb

import "strings"

// SplitPathspec peels a trailing `-- <path>...` off an argument list, the way
// git does, and returns the arguments before it plus the joined path.
//
// It runs BEFORE flags are read, because everything after `--` is a pathspec
// and must not be offered to the flag parser: `git diff -- --weird-name` names
// a file, not an option.
//
// This was two byte-identical copies, splitPathspec in cmd/harness-cli/git.go
// and splitPathspecTokens in tui/cmdline.go, which is the same disease as the
// duplicated ParsePermuted: a helper copied because the packages could not
// see each other, and now they can.
func SplitPathspec(args []string) ([]string, string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], strings.Join(args[i+1:], " ")
		}
	}
	return args, ""
}
