//go:build !js

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	"github.com/on-keyday/objtrsf/objproto"
)

// formatAmbiguousCandidates renders the candidate (runner, agent-profile) combos
// for the non-TUI CLI: a table plus a per-row hint to re-run pinned. Each row
// shows its agent profile — on a single multi-profile runner the rows share a
// cid and differ only by agent, so --runner alone cannot disambiguate and the
// hint must include --agent.
func formatAmbiguousCandidates(cands []cli.RunnerCandidate) string {
	var b strings.Builder
	sameCid := true
	for _, c := range cands[1:] {
		if c.Cid != cands[0].Cid {
			sameCid = false
			break
		}
	}
	if sameCid && len(cands) > 1 {
		fmt.Fprintf(&b, "ambiguous agent: %d profiles on this runner; re-run with --agent <name>:\n", len(cands))
	} else {
		fmt.Fprintf(&b, "ambiguous runner: %d (runner, agent) candidates match this repo; re-run pinned with the flags shown:\n", len(cands))
	}
	for _, c := range cands {
		profile := c.Profile
		if profile == "" {
			profile = "(default)"
		}
		hint := ""
		if !sameCid {
			hint = "--runner " + c.Cid
		}
		if c.Profile != "" {
			if hint != "" {
				hint += " "
			}
			hint += "--agent " + c.Profile
		}
		fmt.Fprintf(&b, "  %-18s agent=%-10s [%d/%d]  %s  %s\n", c.Hostname, profile, c.ActiveTasks, c.MaxTasks, c.MatchedRoot, hint)
	}
	return b.String()
}

// exitOnAmbiguous prints the candidate table and exits non-zero when err is an
// AmbiguousRunnerError; otherwise returns err unchanged.
func exitOnAmbiguous(err error) error {
	var are *cli.AmbiguousRunnerError
	if errors.As(err, &are) {
		fmt.Fprint(os.Stderr, formatAmbiguousCandidates(are.Candidates))
		// 3 = ambiguous runner (distinct from 1=generic, 2=usage).
		os.Exit(3)
	}
	return err
}

// runSession dispatches session sub-verbs: new / attach / snapshot / send /
// exec / ls / kill / await-idle / resize, plus the `stream` NAMESPACE (a third
// level, one verb per inbound kind of the adapter protocol). cid is the
// already-resolved server ConnectionID from main()'s parseCID().
func runSession(cid objproto.ConnectionID, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: harness-cli session <new|attach|snapshot|send|exec|ls|kill|await-idle|resize|stream> [args]")
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
	case "await-idle":
		return runSessionAwaitIdle(cid, rest)
	case "resize":
		return runSessionResize(cid, rest)
	case "stream":
		return runSessionStream(cid, rest)
	default:
		return fmt.Errorf("unknown session verb %q", verb)
	}
}

// runSessionStream dispatches the `session stream <verb>` namespace — the
// event-stream kind's data-plane verbs, one per inbound kind of the adapter
// protocol (design §3). Lifecycle verbs (new/ls/kill) stay on `session`
// because their meaning is kind-independent; these exist because their PTY
// namesakes mean something else (attach splices a terminal) or nothing at all.
//
// `requests` and `snapshot` are the two still unbuilt; naming one reports
// exactly that instead of "unknown", so the design stays discoverable.
func runSessionStream(cid objproto.ConnectionID, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: harness-cli session stream <attach|turn|approve|interrupt|finish> <id> [args]  (requests/snapshot: specified, not built yet)")
		os.Exit(2)
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "attach":
		return runSessionStreamAttach(cid, rest)
	case "turn":
		return runSessionStreamTurn(cid, rest)
	case "approve":
		return runSessionStreamApprove(cid, rest)
	case "interrupt", "finish":
		return runSessionStreamSimple(cid, rest, verb)
	case "requests", "snapshot":
		return fmt.Errorf("session stream %s: specified (design §3) but not built yet; "+
			"`session snapshot --raw` reads this kind's stream verbatim in the meantime", verb)
	default:
		return fmt.Errorf("unknown session stream verb %q", verb)
	}
}

// runSessionStreamTurn appends one user turn.
//
// Flags strictly BEFORE <id>, and everything after it joined ssh-style — the
// same shape `session send` and `session exec` use, for the reason
// parsePermuted's own doc gives: a free-form text positional can begin with
// '-', which is indistinguishable from a flag, so the permuted parse must not
// be used here.
func runSessionStreamTurn(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session stream turn", flag.ExitOnError)
	flushMs := fs.Uint("flush-ms", 400, "ms to let the line drain to the runner before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf(`usage: session stream turn [--flush-ms MS] <id> <text>...
flags must precede <id>; everything after <id> is joined with spaces and sent as
one user turn (ssh-style), so multi-word text needs no quoting.

This is the structured counterpart of ` + "`session send`" + `: it builds the
adapter-protocol line and appends the newline that frames it. ` + "`session send`" + `
stays the raw route and appends nothing — a line without a newline sits in the
adapter's buffer, invisible, until something else flushes it.`)
	}
	taskIDHex := fs.Arg(0)
	text := strings.Join(fs.Args()[1:], " ")

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.StreamTurn(ctx, taskIDHex, text, time.Duration(*flushMs)*time.Millisecond)
}

// runSessionStreamApprove answers one pending request.
//
// The deny reason is a FLAG rather than a trailing positional, which keeps both
// positionals hex-shaped and so lets parsePermuted give this verb order-free
// flags — `approve <id> <req> --deny --message "…"` and `approve --deny <id>
// <req>` both work. A trailing free-form positional would have forced
// flags-first, and this is the verb where a misplaced --allow/--deny would be
// worst: it decides the answer.
func runSessionStreamApprove(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session stream approve", flag.ExitOnError)
	allow := fs.Bool("allow", false, "run the tool as requested")
	deny := fs.Bool("deny", false, "refuse it")
	message := fs.String("message", "", "with --deny, the reason. It reaches the AGENT verbatim as a failed tool result — operator-authored text entering a model's context, not a private note")
	suggestion := fs.Int("suggestion", -1, "accept the request's Nth suggestion (0-based) as well; a suggestion is a STANDING change (e.g. stop asking for this tool), not an answer to this one call")
	flushMs := fs.Uint("flush-ms", 400, "ms to let the line drain to the runner before detaching")
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf(`usage: session stream approve <id> <request-id> --allow | --deny [--message "reason"]
The request id is the staleness guard: the adapter refuses an id it is not
holding, so an answer aimed at a request that has gone is REFUSED rather than
applied to whatever happens to be pending now. Read the pending request with
` + "`session snapshot --raw <id>`" + ` (its input rides the stream verbatim; the
task log's rendering drops it).`)
	}
	if *allow == *deny {
		return fmt.Errorf("session stream approve: give exactly one of --allow or --deny")
	}
	if *allow && *message != "" {
		return fmt.Errorf("session stream approve: --message is the DENY reason; an allow carries no message")
	}
	resp := streamagent.Response{ID: pos[1]}
	if *allow {
		resp.Behavior = streamagent.BehaviorAllow
	} else {
		resp.Behavior = streamagent.BehaviorDeny
		resp.Message = *message
	}
	if *suggestion >= 0 {
		s := *suggestion
		resp.AcceptSuggestion = &s
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.StreamApprove(ctx, pos[0], resp, time.Duration(*flushMs)*time.Millisecond)
}

// runSessionStreamSimple serves the two verbs that carry no payload.
//
// interrupt abandons the running TURN and the agent survives to take the next
// one; finish closes the agent's stdin so it completes the turn in flight and
// exits 0. Neither is `session kill`, which is a signal and discards the work.
func runSessionStreamSimple(cid objproto.ConnectionID, args []string, verb string) error {
	fs := flag.NewFlagSet("session stream "+verb, flag.ExitOnError)
	flushMs := fs.Uint("flush-ms", 400, "ms to let the line drain to the runner before detaching")
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: session stream %s <id>", verb)
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	flush := time.Duration(*flushMs) * time.Millisecond
	if verb == "interrupt" {
		return c.StreamInterrupt(ctx, pos[0], flush)
	}
	return c.StreamFinish(ctx, pos[0], flush)
}

// runSessionStreamAttach follows an event-stream task's events, rendered as
// text — the live counterpart of reading its task log, plus the ring replay.
// Read-only; Ctrl+C detaches and the task keeps running.
func runSessionStreamAttach(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session stream attach", flag.ExitOnError)
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: session stream attach <id>")
	}
	taskIDHex := pos[0]

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	return c.SessionStreamAttach(ctx, taskIDHex, os.Stdout, os.Stderr)
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
	asJSON := fs.Bool("json", false, "emit the screen as one JSON object {task,rows,cols,title,attrs,color,lines[],spans[]} instead of text — lines[] is the grid one row per entry and each span carries row/start/end/attrs/fg/bg, so a reader indexes lines[span.row] instead of parsing the `--- styles ---` report")
	detect := fs.Bool("detect", false, "also judge what STATE the screen shows (working / blocked / idle / unknown) and print the rule and the text it read. blocked means waiting on a HUMAN, which byte-quiescence cannot tell from thinking; with --json the full per-rule explain rides along")
	detectAgent := fs.String("detect-agent", "claude", "with --detect: which agent's rule set to judge by")
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: session snapshot [--rows N --cols N --settle-ms MS] [--style] [--color] [--detect [--detect-agent NAME]] [--json] [--raw] <id>")
	}
	// A typed option that silently does nothing is the failure this repo keeps
	// re-fixing, so naming an agent without asking for detection is an error
	// rather than a no-op.
	if !*detect && flagExplicitlySet(fs, "detect-agent") {
		return fmt.Errorf("--detect-agent takes effect only with --detect")
	}
	if *raw && (*style || *colorOut) {
		return fmt.Errorf("--raw cannot be combined with --style/--color (those report the VT render, which --raw bypasses)")
	}
	// Same axis as the check above: --json is an ENCODING of the VT render,
	// --raw is a different artifact (replay bytes). Base64-wrapping them into
	// the object would invent a third thing rather than format an existing one.
	if *raw && *asJSON {
		return fmt.Errorf("--raw cannot be combined with --json (--json encodes the VT render; --raw emits replay bytes — redirect --raw to a file instead)")
	}
	// Same axis again: detection reads the RENDER (and the title captured while
	// rendering), which --raw never produces.
	if *raw && *detect {
		return fmt.Errorf("--raw cannot be combined with --detect (detection judges the VT render; --raw emits replay bytes)")
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

	opts := screenOpts{
		rows: uint16(*rows), cols: uint16(*cols),
		settle:    time.Duration(*settleMs) * time.Millisecond,
		withAttrs: *style, withColor: *colorOut, asJSON: *asJSON,
	}
	if *detect {
		opts.detectAgent = *detectAgent
	}
	return printSessionScreen(ctx, c, taskIDHex, opts)
}

// screenOpts is what the two snapshot-rendering commands agree on. A struct
// rather than a widening parameter list: this function is called from
// `session snapshot` and `session send --snapshot`, and every option added as
// a positional has to be threaded through both correctly.
type screenOpts struct {
	rows, cols uint16
	settle     time.Duration
	withAttrs  bool
	withColor  bool
	asJSON     bool
	// detectAgent selects the state-detection rule set; "" leaves detection off.
	detectAgent string
}

// printSessionScreen renders taskIDHex's current screen to stdout — plain, or
// followed by the attribute/color span report when asked, or as one JSON object
// when asJSON.
//
// Shared by `session snapshot` and `session send --snapshot` so the two cannot
// drift into two renderings of the same screen. `--raw` deliberately stays with
// snapshot alone: it bypasses the VT render entirely and emits replay BYTES,
// which is a different artifact, not a formatting option.
//
// asJSON changes only the ENCODING, never the content: withAttrs/withColor keep
// selecting which style dimensions are collected, and the object reports both
// back so a reader can tell "no spans because none were asked for" from "asked
// for and none present".
func printSessionScreen(ctx context.Context, c *cli.Client, taskIDHex string, o screenOpts) error {
	// Detection needs the title, which only the structured capture keeps, so
	// asking for it selects that path regardless of the output encoding.
	if o.asJSON || o.detectAgent != "" {
		snap, err := c.SessionSnapshotStructured(ctx, taskIDHex, o.rows, o.cols, o.settle, o.withAttrs, o.withColor)
		if err != nil {
			return err
		}
		var verdict *cli.DetectExplain
		if o.detectAgent != "" {
			set, err := detectRuleSet(o.detectAgent)
			if err != nil {
				return err
			}
			v := snap.Detect(set)
			verdict = &v
		}
		if o.asJSON {
			return json.NewEncoder(os.Stdout).Encode(struct {
				*cli.ScreenSnapshot
				Detect *cli.DetectExplain `json:"detect,omitempty"`
			}{snap, verdict})
		}
		fmt.Println(strings.TrimRight(strings.Join(snap.Lines, "\n"), "\n"))
		if o.withAttrs || o.withColor {
			fmt.Println("\n--- styles ---")
			fmt.Println(formatSpanReport(snap))
		}
		printDetectReport(verdict)
		return nil
	}

	if o.withAttrs || o.withColor {
		text, report, err := c.SessionSnapshotStyled(ctx, taskIDHex, o.rows, o.cols, o.settle, o.withAttrs, o.withColor)
		if err != nil {
			return err
		}
		fmt.Println(strings.TrimRight(text, "\n"))
		fmt.Println("\n--- styles ---")
		fmt.Println(report)
		return nil
	}

	snap, err := c.SessionSnapshot(ctx, taskIDHex, o.rows, o.cols, o.settle)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimRight(snap, "\n"))
	return nil
}

// detectRuleSet resolves an agent name to its rules, naming what IS available
// when it misses — a bare "unknown agent" leaves the caller guessing whether
// they typed it wrong or the rules simply do not exist yet.
func detectRuleSet(agent string) (cli.DetectRuleSet, error) {
	sets, err := cli.DetectRuleSets()
	if err != nil {
		return cli.DetectRuleSet{}, err
	}
	set, ok := sets[agent]
	if !ok {
		have := make([]string, 0, len(sets))
		for name := range sets {
			have = append(have, name)
		}
		sort.Strings(have)
		return cli.DetectRuleSet{}, fmt.Errorf(
			"--detect-agent %q: no rules for that agent (have: %s)", agent, strings.Join(have, ", "))
	}
	return set, nil
}

// printDetectReport writes the human form of a verdict: the state, the rule
// that produced it, and the text that rule read. The per-rule detail stays in
// --json, the same split `session snapshot --style` already uses.
func printDetectReport(v *cli.DetectExplain) {
	if v == nil {
		return
	}
	fmt.Println("\n--- detect ---")
	fmt.Printf("state:  %s\n", v.State)
	fmt.Printf("agent:  %s (rules %s)\n", v.Agent, v.Version)
	switch {
	case v.MatchedRule != "":
		for _, r := range v.Rules {
			if r.ID != v.MatchedRule {
				continue
			}
			fmt.Printf("rule:   %s (region=%s priority=%d)\n", r.ID, r.Evidence.Region, r.Priority)
			fmt.Printf("read:   %q\n", r.Evidence.RegionPreview)
		}
	default:
		// Naming the reason is the point: without it `unknown` and a confident
		// verdict are the same line.
		fmt.Printf("rule:   none (%s)\n", v.FallbackReason)
	}
}

// formatSpanReport renders a captured snapshot's spans in the `--- styles ---`
// form, so the structured path prints what the text path would have.
func formatSpanReport(s *cli.ScreenSnapshot) string {
	return cli.FormatScreenSpans(s.Spans)
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
	resumeConversation := fs.Bool("resume-conversation", false, "with --resume, also ask the runner to resume the agent's own conversation state")
	capsFlag := fs.String("caps", "", cli.CapsFlagUsage)
	scopeFlag := fs.String("scope", "", "which tasks this task's capabilities may target: "+cli.ScopeGrammar+"; default subtree (self + descendants). With --resume, --scope re-grants the scope (omitted = keep the task's), independently of --caps")
	var scopeFor scopeForFlag
	fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage)
	agent := fs.String("agent", "", "agent profile name (empty = runner default)")
	var extraArgs repeatableStrings
	fs.Var(&extraArgs, "agent-arg", "extra CLI arg to forward to the agent (repeatable; appended after runner-global --agent-args)")
	fs.Var(&extraArgs, "claude-arg", "deprecated alias for --agent-arg")
	detach := false
	fs.BoolVar(&detach, "detach", false, "start the session and immediately detach (run in background, print task id, exit)")
	fs.BoolVar(&detach, "d", false, "shorthand for --detach")
	stream := fs.Bool("stream", false,
		"open an EVENT-STREAM session instead of a PTY one: the runner runs the "+
			"profile's stream adapter and the session carries structured events "+
			"rather than terminal bytes. Requires a profile with a stream adapter "+
			"configured; the runner refuses rather than falling back to a terminal")
	x11 := false
	fs.BoolVar(&x11, "x11", false, "forward X11: inject DISPLAY/XAUTHORITY so GUI apps in the session render on your local X server (requires xauth + a running local X server)")
	x11Display := fs.Int("x11-display", 10, "X11 display number N (runner binds 127.0.0.1:6000+N; default 10)")
	// Sizes the session's PTY, unlike `session snapshot --rows/--cols`, which
	// sizes the offscreen renderer and never touches the PTY. Both must be
	// given to take effect. Matters most with -d: a detached session's PTY has
	// no size at all until a client attaches, and a resize needs a CONTROL attach (exec_control),
	// which the spawner may not hold.
	rows := fs.Uint("rows", 0, "initial PTY rows for the session (0 = unset; needs --cols too)")
	cols := fs.Uint("cols", 0, "initial PTY columns for the session (0 = unset; needs --rows too)")
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
	if *stream && x11 {
		return fmt.Errorf("session new: --stream is incompatible with --x11 (X11 is a terminal-session concept; the server refuses the pair too)")
	}

	scope, err := cli.ParseScope(*scopeFlag)
	if err != nil {
		return fmt.Errorf("session new: --scope: %w", err)
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
	sopts := cli.SessionOpts{
		Selector: sel, ExtraArgs: []string(extraArgs), ResumeTaskID: *resume,
		Caps: caps, Scope: scope, ResumeCapsOverride: resumeCapsOverride,
		Overrides:          scopeFor.out,
		EventStream:        *stream,
		ScopePresent:       *resume != "" && flagExplicitlySet(fs, "scope"),
		ResumeConversation: *resumeConversation, AgentProfile: *agent,
		InitialRows: uint16(*rows), InitialCols: uint16(*cols),
	}

	if detach {
		stream, taskIDHex, err := c.OpenInteractive(ctx, repoVal, sopts)
		if err != nil {
			return exitOnAmbiguous(err)
		}
		_ = stream.Close() // immediately detach → server transitions Running -> Detached
		// This process exits on the next line, and exiting takes the transport
		// down with it. sendStream.Close() only FLAGS eof — anything still in
		// the send buffer (notably the initial window-size frame written by
		// OpenInteractive) has not left yet, so exiting here silently drops it.
		// Measured: `session new --rows 40 --cols 150 -d` produced a PTY of
		// `0 0`, and a blind 300ms sleep in this spot produced `40 150`.
		// Completed() is the real condition that sleep was approximating: eof
		// sent AND every sent range acknowledged.
		waitStreamCompleted(stream, 2*time.Second)
		fmt.Println(taskIDHex)
		return nil
	}

	if x11 {
		id, err := c.RunInteractiveX11(ctx, repoVal, sopts, *x11Display)
		if err != nil {
			return exitOnAmbiguous(err)
		}
		fmt.Printf("session %s ended\n", id)
		return nil
	}

	if *stream {
		// The non-detach interactive path hands the local TERMINAL to the new
		// session (raw mode + byte splice), which for this kind would paint
		// raw NDJSON. "Open and stay" for an event-stream session means open
		// detached, then FOLLOW the events — same rendering as
		// `session stream attach`, which is also the command to come back with.
		st, taskIDHex, err := c.OpenInteractive(ctx, repoVal, sopts)
		if err != nil {
			return exitOnAmbiguous(err)
		}
		_ = st.Close()
		waitStreamCompleted(st, 2*time.Second)
		fmt.Fprintf(os.Stderr, "harness-cli: stream session %s started\n", taskIDHex)
		return c.SessionStreamAttach(ctx, taskIDHex, os.Stdout, os.Stderr)
	}

	id, err := c.Interactive(ctx, repoVal, sopts)
	if err != nil {
		return exitOnAmbiguous(err)
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
// (non-takeover, no size authority). --snapshot renders the resulting screen in
// the same call, which is the whole drive loop — send keystrokes, read what the
// program made of them — without a second dial or a guessed sleep.
func runSessionSend(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session send", flag.ExitOnError)
	enter := fs.Bool("enter", false, "append a carriage return (Enter) after the text")
	interp := fs.Bool("e", false, `interpret backslash escapes (\n \r \t \e \xHH \\)`)
	flushMs := fs.Uint("flush-ms", 400, "ms to let the input drain to the runner before detaching")
	quiet := fs.Bool("quiet", false, "suppress the one-line summary of what was sent (stderr)")
	// send→snapshot is the documented way to drive a non-shell foreground, and
	// running it as two commands means two dials and a guessed sleep between
	// them. --snapshot folds the read into this invocation on the SAME client:
	// send, let the input drain, then view-attach and render.
	//
	// Opt-in, because `send` writes nothing to stdout today (its summary goes
	// to stderr) and a caller piping it must keep getting that.
	snapshot := fs.Bool("snapshot", false, "after sending, render the session's screen to stdout — the same view-attach render `session snapshot` prints")
	rows := fs.Uint("rows", 40, "with --snapshot: fallback rows when the session reports no size (sizes the offscreen renderer only, never the PTY)")
	cols := fs.Uint("cols", 120, "with --snapshot: fallback cols when the session reports no size (sizes the offscreen renderer only, never the PTY)")
	settleMs := fs.Uint("settle-ms", 1500, "with --snapshot: ms to collect output before rendering — the window the program has to react to what was just sent")
	// Sizing then driving is one intent, and doing it as two commands lets the
	// program receive keystrokes before it knows how big it is — a full-screen
	// TUI then paints at the wrong size or refuses to paint at all. Applied
	// BEFORE the text for exactly that reason.
	//
	// Spelled ROWSxCOLS rather than reusing --rows/--cols, which on this very
	// command already mean the offscreen RENDER size for --snapshot.
	resize := fs.String("resize", "", "before sending, set the session's PTY size to ROWSxCOLS (e.g. 40x150) — needs exec_resize and an unattached control seat; fails the command if it does not take")
	style := fs.Bool("style", false, "with --snapshot: also print attribute spans (faint/bold/reverse/...) — the plain render drops SGR, so a faint placeholder reads like real typed text and WHICH ROW IS SELECTED is invisible without this")
	// --color and --json exist here for the same reason --style does: this
	// command renders through the very same printSessionScreen, so a flag it
	// understands that this command does not accept is a hole an agent finds by
	// having its drive loop fall back to a second dial. They carry `session
	// snapshot`'s meanings unchanged.
	colorOut := fs.Bool("color", false, "with --snapshot: also print fg/bg colour spans (hex) — verbose (most cells carry a colour); same flag as on `session snapshot`")
	asJSON := fs.Bool("json", false, "with --snapshot: emit the screen as one JSON object instead of text — same shape as `session snapshot --json`")
	detect := fs.Bool("detect", false, "with --snapshot: also judge the resulting state (working / blocked / idle / unknown) — the drive loop's real question after sending a key")
	detectAgent := fs.String("detect-agent", "claude", "with --detect: which agent's rule set to judge by")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A snapshot-only flag given without --snapshot has no effect, so refuse
	// rather than ignore it: a typed option that silently does nothing is the
	// failure mode this repo keeps re-fixing. fs.Visit reports only the flags
	// actually given, which is what distinguishes "left at the default 40"
	// from "asked for 40".
	if !*snapshot {
		var stray []string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "rows", "cols", "settle-ms", "style", "color", "json", "detect", "detect-agent":
				stray = append(stray, "--"+f.Name)
			}
		})
		if len(stray) > 0 {
			return fmt.Errorf("session send: %s take effect only with --snapshot", strings.Join(stray, ", "))
		}
	}
	if fs.NArg() < 2 {
		return fmt.Errorf(`usage: session send [-enter] [-e] [-quiet] [--flush-ms MS] [--snapshot [--rows N] [--cols N] [--settle-ms MS] [--style] [--color] [--json]] <id> <text>...
  -enter     append a carriage return, i.e. actually SUBMIT the line
  -e         interpret backslash escapes (\n \r \t \e \xHH \\) and append nothing.
             -e '\x03' = Ctrl-C, '\x1b' = Esc, '\x1b[A' = Up. NOT short for -enter:
             without -enter the text is typed onto the prompt and just sits there.
  -quiet     suppress the one-line summary of what was sent (stderr)
  --snapshot after sending, render the screen to stdout (send + session snapshot
             in one call, one dial). --rows/--cols/--settle-ms/--style/--color/
             --json mean what they mean on ` + "`session snapshot`" + ` and require
             --snapshot.
  --resize   ROWSxCOLS: set the PTY size BEFORE sending, so a full-screen
             program is the right size when the keys land. NOT --rows/--cols,
             which on this command size the --snapshot render instead.
flags must precede <id>; everything after <id> is joined with spaces and sent
literally (ssh-style), so multi-word text needs no quoting. Quote it as one
argument to preserve exact whitespace.`)
	}
	taskIDHex := fs.Arg(0)
	var resizeRows, resizeCols uint16
	if *resize != "" {
		var perr error
		resizeRows, resizeCols, perr = parseResizeSpec(*resize)
		if perr != nil {
			return perr
		}
	}
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
	// Before the text, so the program is the right size when the keys land.
	// Hard failure rather than a warning: the caller asked for a size, and
	// driving a TUI that is still 0x0 produces a screenful of nothing that
	// looks like the send failing.
	if resizeRows > 0 {
		applied, rerr := c.SessionResize(ctx, taskIDHex, resizeRows, resizeCols, 2*time.Second)
		if rerr != nil {
			return rerr
		}
		if !applied {
			return fmt.Errorf("session send --resize %dx%d: %s", resizeRows, resizeCols, cli.ResizeRejectedHint)
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "session send: resized to %dx%d before sending\n", resizeRows, resizeCols)
		}
	}
	if err := c.SessionSend(ctx, taskIDHex, data, time.Duration(*flushMs)*time.Millisecond); err != nil {
		return err
	}
	// Disclose what went onto the wire. The harness is a byte pipe with no
	// notion of a "line" or of submission — that lives in whichever program
	// holds the foreground, and differs between a readline shell, an agent TUI
	// and vim — so this reports only what this command did, never what the
	// session made of it. Both branches print: sending WITHOUT a CR is the
	// documented way to drive an agent TUI (text first, CR in a separate call),
	// so this is disclosure, not a warning, and stays silent about whether
	// anything was submitted. It exists because omitting -enter otherwise fails
	// completely silently — the text lands on the prompt and sits there, and a
	// later snapshot renders it echoed on the input line, which reads exactly
	// like it ran. -quiet drops it for callers that only want the exit code.
	// The summary goes to stderr and the screen to stdout, so a caller can pipe
	// one without the other — and --quiet composes with --snapshot rather than
	// suppressing it.
	if *snapshot {
		// Both style dimensions and the encoding pass through unchanged. They
		// default off, so the did-my-keystroke-land check stays terse; what
		// changed is that asking for one here works instead of being dropped.
		opts := screenOpts{
			rows: uint16(*rows), cols: uint16(*cols),
			settle:    time.Duration(*settleMs) * time.Millisecond,
			withAttrs: *style, withColor: *colorOut, asJSON: *asJSON,
		}
		if *detect {
			opts.detectAgent = *detectAgent
		}
		if err := printSessionScreen(ctx, c, taskIDHex, opts); err != nil {
			return err
		}
	}
	if *quiet {
		return nil
	}
	unit := "bytes"
	if len(data) == 1 {
		unit = "byte"
	}
	esc := ""
	if *interp {
		esc = ", escapes interpreted"
	}
	cr := "no trailing CR (-enter not given)"
	if *enter {
		cr = "trailing CR appended (-enter)"
	}
	fmt.Fprintf(os.Stderr, "session send: %d %s%s, %s\n", len(data), unit, esc, cr)
	return nil
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
process exits with the command's exit code (124 timeout, 125 error, 126 the
foreground shell exited). The foreground must be a POSIX shell (bash/zsh/sh);
otherwise use send/snapshot.

exec types into the LIVE foreground shell, so state persists across calls AND
shell-terminating commands bite: exit/exec end the shell (killing the session),
and cd/export carry over to later calls. To test an exit code without killing
the shell, wrap it: bash -c 'exit N' or (exit N).`)
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
			"exit":         res.ExitCode,
			"timed_out":    res.TimedOut,
			"shell_exited": res.ShellExited,
			"duration_ms":  res.Duration.Milliseconds(),
		}
		if !*exitOnly {
			obj["output"] = string(res.Output)
		}
		_ = json.NewEncoder(os.Stdout).Encode(obj)
	} else if !*exitOnly {
		_, _ = os.Stdout.Write(res.Output)
	}

	code := res.ExitCode
	switch {
	case res.ShellExited:
		fmt.Fprintln(os.Stderr, "session exec: the session's foreground shell exited before the command finished — did the command run `exit`/`exec` (or otherwise terminate the shell)? the session is likely now terminal (dead). This is NOT a timeout. Note: exec types into the LIVE foreground shell, so `exit N` kills it; to test an exit code use `bash -c '...'` or a subshell `(exit N)` instead.")
		code = 126
	case res.TimedOut:
		fmt.Fprintf(os.Stderr, "session exec: no completion within %s; the command is still running, or the session foreground is not a POSIX shell (exec needs bash/zsh/sh) — use session send/snapshot instead\n", *timeout)
		code = 124
	case code < 0:
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

// runSessionLs lists interactive sessions as JSON Lines. Each row shares the
// `ls --json` task vocabulary (via cli.SessionListJSON) plus the session-only
// is_attached / ring_buffer_bytes fields, so `session ls` differs from
// `ls --json` only by the interactive filter and those two extra fields.
func runSessionLs(cid objproto.ConnectionID, _ []string) error {
	return cli.SessionListJSON(context.Background(), cid, os.Stdout)
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

// runSessionAwaitIdle arms a one-shot idle watcher on a live interactive
// session. Default sink long-polls (the process blocks until the session's
// PTY output goes quiescent, then prints the result); --notify / --topic arm
// a server-side sink and return immediately — an agent uses
// `--topic chat.<its-short-id>` and ends its turn, the fire arrives via its
// inbox hook.
func runSessionAwaitIdle(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session await-idle", flag.ExitOnError)
	thresholdMs := fs.Uint("threshold-ms", 0, "quiescence threshold in ms (0 = server default 2500)")
	notify := fs.Bool("notify", false, "fire as an operator notification instead of long-polling")
	topic := fs.String("topic", "", "fire as an agentboard message to this topic instead of long-polling")
	// Positional is a hex task id (never '-'-leading), so flag position is free.
	pargs, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pargs) != 1 {
		return fmt.Errorf("usage: session await-idle [--threshold-ms N] [--notify | --topic T] <id>")
	}
	if *notify && *topic != "" {
		return fmt.Errorf("--notify and --topic are mutually exclusive")
	}
	sink := protocol.AwaitIdleSink_Reply
	switch {
	case *notify:
		sink = protocol.AwaitIdleSink_Notify
	case *topic != "":
		sink = protocol.AwaitIdleSink_Board
	}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.AwaitIdle(ctx, pargs[0], uint32(*thresholdMs), sink, *topic)
	if err != nil {
		return err
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"status":         awaitIdleStatusStr(resp.Status),
		"last_output_at": resp.LastOutputAt,
	})
	switch resp.Status {
	case protocol.AwaitIdleStatus_Fired, protocol.AwaitIdleStatus_Armed:
		return nil
	case protocol.AwaitIdleStatus_SessionStopped:
		os.Exit(3) // distinct from fired so scripts can branch
	}
	os.Exit(1) // not_found / bad_request
	return nil
}

// awaitIdleStatusStr renders the wire enum in the snake_case the schema uses
// (the generated String() is CamelCase, which would JSON-encode as
// "SessionStopped").
func awaitIdleStatusStr(s protocol.AwaitIdleStatus) string {
	switch s {
	case protocol.AwaitIdleStatus_Fired:
		return "fired"
	case protocol.AwaitIdleStatus_Armed:
		return "armed"
	case protocol.AwaitIdleStatus_SessionStopped:
		return "session_stopped"
	case protocol.AwaitIdleStatus_NotFound:
		return "not_found"
	case protocol.AwaitIdleStatus_BadRequest:
		return "bad_request"
	default:
		return s.String()
	}
}

// waitStreamCompleted blocks until s reports its data fully sent and
// acknowledged, or until timeout. Polling is the available mechanism: the
// stream exposes Completed() but no completion signal to wait on.
//
// The timeout is a bound, not an expectation — on a healthy link this returns
// in a few milliseconds. Exceeding it means the frames did not make it, which
// is not worth failing the detach over: the session itself is already open and
// usable, only the size hint is lost.
func waitStreamCompleted(s interface{ Completed() bool }, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for !s.Completed() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}

// parseResizeSpec parses "ROWSxCOLS" (e.g. "40x150"). One flag rather than a
// --rows/--cols pair on purpose: `session send --snapshot` and `session
// snapshot` already have --rows/--cols meaning the OFFSCREEN RENDER size, and
// `session new` has them meaning the PTY size. A third spelling of the same two
// words on the same command line is how the existing confusion gets worse.
func parseResizeSpec(spec string) (rows, cols uint16, err error) {
	var r, c int
	if n, _ := fmt.Sscanf(spec, "%dx%d", &r, &c); n != 2 {
		return 0, 0, fmt.Errorf("resize %q: want ROWSxCOLS, e.g. 40x150", spec)
	}
	if r <= 0 || c <= 0 || r > 65535 || c > 65535 {
		return 0, 0, fmt.Errorf("resize %q: rows and cols must be 1..65535", spec)
	}
	return uint16(r), uint16(c), nil
}

// runSessionResize sets a live session's PTY size from a non-control attach.
//
// It reports whether the size TOOK, because the server discards a disallowed
// resize silently — correct for the implicit per-SIGWINCH stream a real
// terminal emits, wrong for someone who typed this command. Exit 3 on "not
// applied" so a script can branch without parsing text.
func runSessionResize(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session resize", flag.ExitOnError)
	spec := fs.String("size", "", "new PTY size as ROWSxCOLS (e.g. 40x150)")
	waitMs := fs.Uint("wait-ms", 2000, "ms to wait for the server to echo the new size back — that echo is the acknowledgement")
	quiet := fs.Bool("quiet", false, "suppress the one-line result on stderr")
	pargs, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if len(pargs) < 1 || *spec == "" {
		return fmt.Errorf(`usage: session resize --size ROWSxCOLS [--wait-ms MS] [-quiet] <id>
Sets the PTY size WITHOUT taking the session over. Needs exec_view to attach and
exec_resize to be honoured, and applies only while no control client is attached
— the control attach owns the size whenever it holds the seat. Exits 3 when the
size was not applied.`)
	}
	rows, cols, err := parseResizeSpec(*spec)
	if err != nil {
		return err
	}
	taskIDHex := pargs[0]

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	applied, err := c.SessionResize(ctx, taskIDHex, rows, cols, time.Duration(*waitMs)*time.Millisecond)
	if err != nil {
		return err
	}
	if !applied {
		fmt.Fprintf(os.Stderr, "session resize: %s\n", cli.ResizeRejectedHint)
		os.Exit(3)
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "session resize: %s now %dx%d (rows x cols)\n", taskIDHex, rows, cols)
	}
	return nil
}
