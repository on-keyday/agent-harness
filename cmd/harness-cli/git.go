package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func runGit(cid objproto.ConnectionID, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: harness-cli git <task-id> {log|diff|show|status|subrepos|file} ...")
	}
	taskID := args[0]
	sub := args[1]
	rest := args[2:]

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	color := isTTY(os.Stdout)
	emit := func(res *cli.GitResult, err error) error {
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return err
		}
		renderGitResult(os.Stdout, res, color)
		return nil
	}

	// Parsed from the declaration (cli/verb): flags, aliases, revision counts
	// and cross-flag validity are written down once, and the TUI and the WebUI
	// read the same entries.
	sp, ok := verb.Lookup("git", sub)
	if !ok {
		return fmt.Errorf("git: unknown sub-verb %q (log | diff | show | status | subrepos | file)", sub)
	}
	sp = sp.For(verb.CLI)
	fs := flagSetFor(sp)
	b, perr := sp.Parse(fs, rest)
	if perr != nil {
		return perr
	}
	act, berr := sp.BuildFunc()(b)
	if berr != nil {
		return berr
	}
	g := act.(verb.GitAction)
	q := cli.GitQuery{
		BaseRev: g.BaseRev, TargetRev: g.TargetRev, Path: g.Path, Subrepo: g.Subrepo,
		MaxCommits: g.Max, MaxBytes: g.MaxBytes, SubmoduleDiff: g.Submodule,
	}
	switch g.Sub {
	case "log":
		return emit(c.GitLog(ctx, taskID, q))
	case "diff":
		if g.Staged {
			q.Target = protocol.GitDiffTarget_Index
		} else if g.TargetRev != "" {
			q.Target = protocol.GitDiffTarget_Rev
		}
		return emit(c.GitDiff(ctx, taskID, q))
	case "show":
		return emit(c.GitShow(ctx, taskID, q))
	case "status":
		return emit(c.GitStatus(ctx, taskID, q))
	case "subrepos":
		return emit(c.GitSubrepos(ctx, taskID, q))
	case "file":
		if g.Staged {
			q.Target = protocol.GitDiffTarget_Index
		} else if g.TargetRev != "" {
			// `git file --rev X` reads the copy AT X, so the revision is the
			// query's base and the target names which side to read.
			q.Target = protocol.GitDiffTarget_Rev
			q.BaseRev = g.TargetRev
			q.TargetRev = ""
		}
		return emit(c.GitFile(ctx, taskID, q))
	}
	return fmt.Errorf("git: unhandled sub-verb %q", g.Sub)
}

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

// flagSetFor builds a verb's FlagSet with the CLI's error mode: a bad command
// line should end the process with usage, unlike the TUI where it is a line in
// a pane.
func flagSetFor(sp verb.VerbSpec) *flag.FlagSet { return sp.NewFlagSet(flag.ExitOnError) }
