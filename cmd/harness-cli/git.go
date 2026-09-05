package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	ansiReset = "\x1b[0m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiCyan  = "\x1b[36m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
)

func gitLineANSI(class cli.GitLineClass) string {
	switch class {
	case cli.GitLineAdd:
		return ansiGreen
	case cli.GitLineDel:
		return ansiRed
	case cli.GitLineHunk:
		return ansiCyan
	case cli.GitLineFile:
		return ansiBold
	case cli.GitLineMeta:
		return ansiDim
	default:
		return ""
	}
}

func renderGitResult(w io.Writer, res *cli.GitResult, color bool) {
	switch res.Kind {
	case protocol.GitQueryKind_Log:
		for _, c := range res.Commits {
			fmt.Fprintf(w, "%s  %s  %-12s  %s\n",
				c.Short(), c.When.Format("2006-01-02 15:04"), c.Author, c.Subject)
		}
		if res.Truncated {
			fmt.Fprintln(w, "(truncated: more commits exist; raise --max)")
		}
	case protocol.GitQueryKind_Status:
		for _, e := range res.Entries {
			fmt.Fprintf(w, "%s %s\n", e.XY, e.Path)
		}
	case protocol.GitQueryKind_File:
		// Whole-file content: no diff colouring, and no trailing-newline
		// fiddling — it goes out as the bytes it is.
		fmt.Fprint(w, res.Text)
		if res.Truncated {
			fmt.Fprintln(w, "\n(truncated: raise --max-bytes to see more)")
		}
	case protocol.GitQueryKind_Subrepos:
		for _, p := range res.Subrepos {
			fmt.Fprintln(w, p)
		}
		if len(res.Subrepos) == 0 {
			fmt.Fprintln(w, "(no nested repositories)")
		}
		if res.Truncated {
			fmt.Fprintln(w, "(truncated: more nested repositories exist than the walk reports)")
		}
	default:
		// A trailing newline in the text would otherwise print as a blank
		// line, which reads as content that is not there.
		text := strings.TrimSuffix(res.Text, "\n")
		if text != "" {
			for _, line := range strings.Split(text, "\n") {
				if !color {
					fmt.Fprintln(w, line)
					continue
				}
				if prefix := gitLineANSI(cli.ClassifyGitLine(line)); prefix != "" {
					fmt.Fprintf(w, "%s%s%s\n", prefix, line, ansiReset)
				} else {
					fmt.Fprintln(w, line)
				}
			}
		}
		if res.Truncated {
			fmt.Fprintln(w, "(truncated: raise --max-bytes to see more)")
		}
	}
}

// isTTY reports whether f is a terminal, which is the only condition under
// which colour escapes belong in the output — a redirected diff has to stay
// byte-clean for `git apply`.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
