//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runSession dispatches session sub-verbs: new / attach / snapshot / send /
// exec / ls / kill. cid is the already-resolved server ConnectionID from
// main()'s parseCID().
func runSession(cid objproto.ConnectionID, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: harness-cli session <new|attach|snapshot|send|exec|ls|kill> [args]")
		os.Exit(2)
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "new":
		return runSessionNew(cid, rest)
	case "attach":
		return runSessionAttach(cid, rest)
	case "snapshot":
		return runSessionSnapshot(cid, rest)
	case "send":
		return runSessionSend(cid, rest)
	case "exec":
		return runSessionExec(cid, rest)
	case "ls":
		return runSessionLs(cid, rest)
	case "kill":
		return runSessionKill(cid, rest)
	default:
		return fmt.Errorf("unknown session verb %q", verb)
	}
}

// parsePermuted parses fs but tolerates flags appearing after positional args.
// Go's stdlib flag stops at the first non-flag token, so `cmd <id> --flag` would
// otherwise silently drop --flag (it lands in fs.Args() and is ignored). We peel
// positionals one at a time and re-parse the remainder, making flag position
// irrelevant — the model can write the flag before or after the id and it works.
//
// Use this ONLY for commands whose positionals can never begin with '-' (e.g. a
// hex task id). For free-form text positionals, keep flags strictly before the
// positional instead: a '-'-leading word is indistinguishable from a flag, and a
// '--' terminator would not survive the peel loop.
func parsePermuted(fs *flag.FlagSet, args []string) ([]string, error) {
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

// runSessionSnapshot view-attaches to a detachable session and prints its
// current screen as plain text (headless VT render). Non-intrusive: it does not
// take over the controlling client. Works from a non-TTY context (no raw mode),
// unlike `session attach`.
func runSessionSnapshot(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session snapshot", flag.ExitOnError)
	rows := fs.Uint("rows", 40, "fallback rows when the session reports no size")
	cols := fs.Uint("cols", 120, "fallback cols when the session reports no size")
	settleMs := fs.Uint("settle-ms", 1500, "ms to collect output before rendering")
	style := fs.Bool("style", false, "also print attribute spans (faint/bold/italic/reverse/...) after the screen — the plain render drops SGR, so a faint placeholder/ghost reads like real input without this")
	colorOut := fs.Bool("color", false, "also print fg/bg color spans (hex) after the screen — verbose (most cells carry a color); combine with or use independently of --style")
	raw := fs.Bool("raw", false, "write the verbatim PTY replay bytes (escape sequences intact) to stdout instead of the VT-rendered screen — cat into a real terminal to reproduce it exactly; --rows/--cols are ignored and --style/--color are not allowed")
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: session snapshot [--rows N --cols N --settle-ms MS] [--style] [--color] [--raw] <id>")
	}
	if *raw && (*style || *colorOut) {
		return fmt.Errorf("--raw cannot be combined with --style/--color (those report the VT render, which --raw bypasses)")
	}
	taskIDHex := pos[0]

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	if *raw {
		b, err := c.SessionSnapshotRaw(ctx, taskIDHex, time.Duration(*settleMs)*time.Millisecond)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}

	if *style || *colorOut {
		text, report, err := c.SessionSnapshotStyled(ctx, taskIDHex, uint16(*rows), uint16(*cols), time.Duration(*settleMs)*time.Millisecond, *style, *colorOut)
		if err != nil {
			return err
		}
		fmt.Println(strings.TrimRight(text, "\n"))
		fmt.Println("\n--- styles ---")
		fmt.Println(report)
		return nil
	}

	snap, err := c.SessionSnapshot(ctx, taskIDHex, uint16(*rows), uint16(*cols), time.Duration(*settleMs)*time.Millisecond)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimRight(snap, "\n"))
	return nil
}

// runSessionNew opens a new detachable interactive PTY session on a runner
// and blocks until the session ends (Ctrl+D / exit / detach).
// With -d / --detach the stream is closed immediately after open and the task
// id is printed — mirroring `docker run -d`.
func runSessionNew(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session new", flag.ExitOnError)
	repo := fs.String("repo", "", "repo path (required; env: HARNESS_REPO_PATH)")
	runner := fs.String("runner", "", "pin to runner by ConnectionID hex")
	host := fs.String("host", "", "pin to runner by hostname")
	ip := fs.String("ip", "", "pin to runner by IP address")
	resume := fs.String("resume", "", "task id (32 hex) of a terminal interactive task to resume into a new detachable session; --repo is ignored")
	capsFlag := fs.String("caps", "", "comma-separated capability names to grant the task (e.g. spawn,file_read / all / none); default: inherit all the spawner holds. With --resume, --caps re-grants caps to the task (else its persisted caps are kept)")
	var extraArgs repeatableStrings
	fs.Var(&extraArgs, "claude-arg", "extra CLI arg to forward to claude (repeatable; appended after runner-global --claude-args)")
	detach := false
	fs.BoolVar(&detach, "detach", false, "start the session and immediately detach (run in background, print task id, exit)")
	fs.BoolVar(&detach, "d", false, "shorthand for --detach")
	x11 := false
	fs.BoolVar(&x11, "x11", false, "forward X11: inject DISPLAY/XAUTHORITY so GUI apps in the session render on your local X server (requires xauth + a running local X server)")
	x11Display := fs.Int("x11-display", 10, "X11 display number N (runner binds 127.0.0.1:6000+N; default 10)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoVal := *repo
	if repoVal == "" {
		repoVal = os.Getenv("HARNESS_REPO_PATH")
	}
	if repoVal == "" && *resume == "" {
		return fmt.Errorf("session new: --repo required (or set HARNESS_REPO_PATH) — except when --resume is set, which uses the existing task's repo")
	}

	if x11 && detach {
		return fmt.Errorf("session new: --x11 is incompatible with --detach (a detached session has no client to host the X tunnel)")
	}
	if x11 && (*x11Display < 0 || *x11Display > 99) {
		return fmt.Errorf("session new: --x11-display must be 0..99")
	}

	caps, err := cli.ParseCaps(*capsFlag)
	if err != nil {
		return fmt.Errorf("session new: --caps: %w", err)
	}

	opts := cli.SelectorOpts{Runner: *runner, Host: *host, IP: *ip}
	if err := opts.ValidateSelector(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sel, err := cli.BuildSelector(opts)
	if err != nil {
		return err
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	resumeCapsOverride := *resume != "" && capsExplicitlySet(fs)

	if detach {
		stream, taskIDHex, err := c.OpenInteractiveWithSelectorArgsAndCaps(ctx, repoVal, sel, []string(extraArgs), *resume, true, caps, resumeCapsOverride)
		if err != nil {
			return err
		}
		_ = stream.Close() // immediately detach → server transitions Running -> Detached
		fmt.Println(taskIDHex)
		return nil
	}

	if x11 {
		id, err := c.RunInteractiveX11(ctx, repoVal, sel, []string(extraArgs), *resume, *x11Display, caps, resumeCapsOverride)
		if err != nil {
			return err
		}
		fmt.Printf("session %s ended\n", id)
		return nil
	}

	id, err := c.InteractiveWithSelectorArgsAndCaps(ctx, repoVal, sel, []string(extraArgs), *resume, true /*detachable*/, caps, resumeCapsOverride)
	if err != nil {
		return err
	}
	fmt.Printf("session %s ended\n", id)
	return nil
}

// runSessionAttach re-attaches to a detachable interactive session by id.
// With --view the attach is read-only: the server discards keystrokes from
// this client but continues streaming PTY output.
func runSessionAttach(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session attach", flag.ExitOnError)
	view := fs.Bool("view", false, "attach in view-only mode (output only; your input is discarded by the server)")
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: session attach [--view] <id>")
	}
	taskIDHex := pos[0]

	mode := protocol.AttachMode_Control
	if *view {
		mode = protocol.AttachMode_View
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	if _, err := c.SessionAttach(ctx, taskIDHex, mode); err != nil {
		return err
	}
	return nil
}

// runSessionSend injects input into a session via a co-writer attach
// (non-takeover, no size authority). Pair with `session snapshot` to drive a
// session statelessly: send keystrokes, then snapshot to read the result.
func runSessionSend(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session send", flag.ExitOnError)
	enter := fs.Bool("enter", false, "append a carriage return (Enter) after the text")
	interp := fs.Bool("e", false, `interpret backslash escapes (\n \r \t \e \xHH \\)`)
	flushMs := fs.Uint("flush-ms", 400, "ms to let the input drain to the runner before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf(`usage: session send [--enter] [-e] [--flush-ms MS] <id> <text>...
flags must precede <id>; everything after <id> is joined with spaces and sent
literally (ssh-style), so multi-word text needs no quoting. Quote it as one
argument to preserve exact whitespace.`)
	}
	taskIDHex := fs.Arg(0)
	// Join everything after <id> as the text to send, ssh-style (`ssh host cmd
	// args...`). This matches the common instinct of typing words without
	// quoting; otherwise a stray space would silently drop all but the first
	// word (we only ever read fs.Arg(1) before). Flags stay strictly before
	// <id> so a '-'-leading word here is still sent literally.
	text := strings.Join(fs.Args()[1:], " ")
	data := []byte(text)
	if *interp {
		d, err := unescapeInput(text)
		if err != nil {
			return err
		}
		data = d
	}
	if *enter {
		data = append(data, '\r')
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.SessionSend(ctx, taskIDHex, data, time.Duration(*flushMs)*time.Millisecond)
}

// runSessionExec runs a single shell command line synchronously in a session's
// foreground shell (via a cowrite attach + sentinel-bounded output) and returns
// its combined output plus exit code. Unlike send+snapshot it blocks until the
// command finishes, so no sleep-guessing is needed. The process exits with the
// command's own exit code (124 on timeout, 125 on transport/attach error).
// Flags must precede <id>; everything after <id> is one shell command line.
func runSessionExec(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session exec", flag.ExitOnError)
	timeout := fs.Duration("timeout", 30*time.Second, "max wait for the command to finish before giving up (exit 124)")
	jsonOut := fs.Bool("json", false, `emit {"exit":N,"output":"…","timed_out":bool,"duration_ms":N} as one JSON object`)
	exitOnly := fs.Bool("exit-only", false, "print no output; only propagate the exit code")
	raw := fs.Bool("raw", false, "return the verbatim output bytes (escape sequences intact) instead of the interpreted plain text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf(`usage: session exec [--timeout D] [--json] [--exit-only] [--raw] <id> <cmd>...
flags must precede <id>; everything after <id> is joined with spaces and run as
one shell command line (ssh-style) in the session's foreground shell. The
process exits with the command's exit code (124 on timeout, 125 on error).
The foreground must be a POSIX shell (bash/zsh/sh); otherwise use send/snapshot.`)
	}
	taskIDHex := fs.Arg(0)
	cmd := strings.Join(fs.Args()[1:], " ")

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	res, execErr := c.SessionExec(ctx, taskIDHex, cmd, cli.ExecOptions{Timeout: *timeout, Raw: *raw})
	c.Close()
	if execErr != nil {
		fmt.Fprintln(os.Stderr, "session exec:", execErr)
		os.Exit(125)
	}

	if *jsonOut {
		obj := map[string]any{
			"exit":        res.ExitCode,
			"timed_out":   res.TimedOut,
			"duration_ms": res.Duration.Milliseconds(),
		}
		if !*exitOnly {
			obj["output"] = string(res.Output)
		}
		_ = json.NewEncoder(os.Stdout).Encode(obj)
	} else if !*exitOnly {
		_, _ = os.Stdout.Write(res.Output)
	}

	code := res.ExitCode
	if res.TimedOut {
		fmt.Fprintf(os.Stderr, "session exec: no completion within %s; the session foreground may not be a POSIX shell (exec needs bash/zsh/sh) — use session send/snapshot instead\n", *timeout)
		code = 124
	} else if code < 0 {
		code = 125
	}
	os.Exit(code)
	return nil // unreachable
}

// unescapeInput expands a small set of backslash escapes for sending control
// keys: \n \r \t \e (ESC) \\ and \xHH (one byte).
func unescapeInput(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		i++
		if i >= len(s) {
			return nil, fmt.Errorf("trailing backslash")
		}
		switch s[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'e':
			out = append(out, 0x1b)
		case '\\':
			out = append(out, '\\')
		case 'x':
			if i+2 >= len(s) {
				return nil, fmt.Errorf(`\x needs 2 hex digits`)
			}
			b, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return nil, fmt.Errorf(`bad \x escape: %w`, err)
			}
			out = append(out, byte(b))
			i += 2
		default:
			return nil, fmt.Errorf(`unknown escape \%c`, s[i])
		}
	}
	return out, nil
}

// runSessionLs lists detachable interactive sessions as JSON Lines.
func runSessionLs(cid objproto.ConnectionID, _ []string) error {
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	for _, t := range lr.Tasks {
		if t.Kind != protocol.TaskKind_Interactive || !t.Detachable() {
			continue
		}
		_ = enc.Encode(map[string]any{
			"id":                hex.EncodeToString(t.Id.Id[:]),
			"status":            t.Status.String(),
			"is_attached":       t.IsAttached(),
			"repo":              string(t.RepoPath),
			"runner":            protocol.RunnerIDToConnID(t.AssignedTo).String(),
			"created_at":        t.CreatedAt,
			"started_at":        t.StartedAt,
			"ring_buffer_bytes": t.RingBufferBytes,
		})
	}
	return nil
}

// runSessionKill cancels a session (alias of 'harness-cli cancel <id>').
func runSessionKill(cid objproto.ConnectionID, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: session kill <id>")
	}
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Cancel(ctx, args[0])
}
