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
	"github.com/on-keyday/agent-harness/cli/verb"
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

// runSessionStreamTurnWith is runSessionStreamTurn for a caller that
// already has the parsed action -- the generated CLI dispatch.
func runSessionStreamTurnWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	taskIDHex, text := a.TaskID, a.Text
	flushMs := &a.FlushMs

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.StreamTurn(ctx, taskIDHex, text, time.Duration(*flushMs)*time.Millisecond)
}

// runSessionStreamApproveWith is runSessionStreamApprove for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionStreamApproveWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	resp := streamagent.Response{ID: a.RequestID}
	if a.Allow {
		resp.Behavior = streamagent.BehaviorAllow
	} else {
		resp.Behavior = streamagent.BehaviorDeny
		resp.Message = a.Message
	}
	// Outside the verdict: a suggestion is a STANDING change ("stop asking for
	// this tool"), so it accompanies a deny as readily as an allow. The TUI
	// (tui/chat.go) and the WebUI both send it; the CLI stopped being able to
	// when this flag was re-read as a JSON payload.
	if a.SuggestionSet {
		n := int(a.Suggestion)
		resp.AcceptSuggestion = &n
	}
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.StreamApprove(ctx, a.TaskID, resp, time.Duration(a.FlushMs)*time.Millisecond)
}

// runSessionStreamSimpleWith is runSessionStreamSimple for a caller that
// already has the parsed action. interrupt and finish differ only in the
// Sub the declaration fixed, which the body reads off the action.
func runSessionStreamSimpleWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	flush := time.Duration(a.FlushMs) * time.Millisecond
	pos := []string{a.TaskID}

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	// The declaration fixes Sub per path, so the two spellings are told apart
	// by the table rather than by a string this function was handed.
	if a.Sub == "stream-interrupt" {
		return c.StreamInterrupt(ctx, pos[0], flush)
	}
	return c.StreamFinish(ctx, pos[0], flush)
}

// runSessionStreamAttachWith is runSessionStreamAttach for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionStreamAttachWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	taskIDHex := a.TaskID
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	return c.SessionStreamAttach(ctx, taskIDHex, os.Stdout, os.Stderr)
}

// runSessionSnapshotWith is runSessionSnapshot for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionSnapshotWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	taskIDHex := a.TaskID
	rows, cols, settleMs := &a.Rows, &a.Cols, &a.SettleMs
	style, colorOut, withoutSynth := &a.Style, &a.Color, &a.WithoutSynth
	raw, asJSON, ansi := &a.Raw, &a.JSON, &a.ANSI
	detect, detectAgent := &a.Detect, &a.DetectAgent
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	if *raw {
		b, synth, err := c.SessionSnapshotRaw(ctx, taskIDHex,
			time.Duration(*settleMs)*time.Millisecond, !*withoutSynth)
		if err != nil {
			return err
		}
		// The note goes to stderr, so a pipe still receives exactly the bytes
		// asked for. Saying it rather than staying silent is the point: a reader
		// who reached for raw output because the render looked wrong needs to
		// know whether what they are holding is only the PTY's bytes, and that
		// --without-synth exists when the PTY's own bytes are the subject.
		// Reported in BOTH directions and gated on synth > 0 — on whether there
		// were any such bytes, never on whether they were kept.
		switch {
		case synth > 0 && !*withoutSynth:
			fmt.Fprintf(os.Stderr, "harness-cli: %d of these bytes are server-synthesised replay "+
				"(mode preamble and screen repaint), not PTY output\n", synth)
		case synth > 0:
			fmt.Fprintf(os.Stderr, "harness-cli: withheld %d bytes of server-synthesised replay "+
				"(mode preamble and screen repaint); the screen the server would have "+
				"painted is not in this output\n", synth)
		}
		_, err = os.Stdout.Write(b)
		return err
	}

	opts := screenOpts{
		rows: uint16(*rows), cols: uint16(*cols),
		settle:    time.Duration(*settleMs) * time.Millisecond,
		withAttrs: *style, withColor: *colorOut, asJSON: *asJSON, ansi: *ansi,
		includeSynth: !*withoutSynth,
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
	// ansi paints the screen instead of flattening it: the same render, emitted
	// with the SGR escapes that produced it.
	ansi bool
	// detectAgent selects the state-detection rule set; "" leaves detection off.
	detectAgent string
	// includeSynth feeds the emulator the bytes the SERVER synthesised for the
	// replay as well as the PTY's own — true for every ordinary render, because
	// the screen repaint is what reconstructs a session whose opening bytes the
	// ring has evicted. False is the debugging view: what the PTY alone drew.
	includeSynth bool
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
	// --ansi carries its own capture because it needs the cells, not the
	// flattened rows — and it still yields the structured form, so a detection
	// verdict can accompany the picture rather than costing a second capture of
	// a screen that has moved on since.
	if o.ansi {
		painted, snap, err := c.SessionSnapshotANSI(ctx, taskIDHex, o.rows, o.cols, o.settle, o.includeSynth)
		if err != nil {
			return err
		}
		fmt.Println(painted)
		if o.withAttrs || o.withColor {
			fmt.Println("\n--- styles ---")
			fmt.Println(formatSpanReport(snap))
		}
		if o.detectAgent != "" {
			set, err := detectRuleSet(o.detectAgent)
			if err != nil {
				return err
			}
			v := snap.Detect(set)
			printDetectReport(&v, snap.Live)
		}
		return nil
	}

	// Detection needs the title, which only the structured capture keeps, so
	// asking for it selects that path regardless of the output encoding.
	if o.asJSON || o.detectAgent != "" {
		snap, err := c.SessionSnapshotStructured(ctx, taskIDHex, o.rows, o.cols, o.settle, o.withAttrs, o.withColor, o.includeSynth)
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
		printDetectReport(verdict, snap.Live)
		return nil
	}

	if o.withAttrs || o.withColor {
		text, report, err := c.SessionSnapshotStyled(ctx, taskIDHex, o.rows, o.cols, o.settle, o.withAttrs, o.withColor, o.includeSynth)
		if err != nil {
			return err
		}
		fmt.Println(strings.TrimRight(text, "\n"))
		fmt.Println("\n--- styles ---")
		fmt.Println(report)
		return nil
	}

	snap, err := c.SessionSnapshot(ctx, taskIDHex, o.rows, o.cols, o.settle, o.includeSynth)
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
func printDetectReport(v *cli.DetectExplain, live cli.LiveActivity) {
	if v == nil {
		return
	}
	fmt.Println("\n--- detect ---")
	// Printed beside the verdict rather than with the screen because it answers
	// the same question the verdict does, from the one input a pane border, a
	// resize or an unrecognised UI cannot corrupt. It is NOT part of the
	// verdict: no rule reads it yet, so a reader has to weigh it themselves,
	// and saying so here is cheaper than having them assume either way.
	fmt.Printf("live:   %d frames / %d B in %d ms (no rule reads this yet)\n",
		live.Frames, live.Bytes, live.WindowMs)
	if !live.Anchored {
		fmt.Println("        UNANCHORED: no end-of-replay repaint arrived, so the window still" +
			" holds replayed history delivered at wire speed — the counts above are not a rate")
	}
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

// runSessionNewWith is runSessionNew for a caller that already has the
// parsed action -- the generated CLI dispatch.
func runSessionNewWith(cid objproto.ConnectionID, a verb.SpawnAction) error {
	repoVal := a.Repo
	sopts := spawnOpts(a)
	sopts.EventStream = a.Stream
	sopts.InitialRows, sopts.InitialCols = uint16(a.Rows), uint16(a.Cols)
	detach, x11, x11Display := a.Detach, a.X11, int(a.X11Display)

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

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
		id, err := c.RunInteractiveX11(ctx, repoVal, sopts, x11Display)
		if err != nil {
			return exitOnAmbiguous(err)
		}
		fmt.Printf("session %s ended\n", id)
		return nil
	}

	if a.Stream {
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

// runSessionAttachWith is runSessionAttach for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionAttachWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	taskIDHex := a.TaskID
	// --view attaches without taking the seat: output only, input discarded.
	mode := protocol.AttachMode_Control
	if a.View {
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
	// Parsed from the declaration. --enter and -e stay two flags there, and a
	// test refuses to let them become one: merging them would type a spurious
	// Enter into a live PTY.
	sp, _ := verb.Lookup("session", "send")
	sp = sp.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ExitOnError)
	b, perr := sp.Parse(fs, args)
	if perr != nil {
		return perr
	}
	act, berr := sp.BuildFunc()(b)
	if berr != nil {
		return berr
	}
	a := act.(verb.SendAction)
	return runSessionSendWith(cid, a)
}

// runSessionSendWith is runSessionSend for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionSendWith(cid objproto.ConnectionID, a verb.SendAction) error {
	taskIDHex := a.TaskID
	enter, interp, quiet := &a.Enter, &a.Interp, &a.Quiet
	flushMs := &a.FlushMs
	var resizeRows, resizeCols uint16
	if a.Resize != "" {
		var perr error
		resizeRows, resizeCols, perr = parseResizeSpec(a.Resize)
		if perr != nil {
			return perr
		}
	}
	snapshot, ansi, asJSON := &a.Snapshot, &a.ANSI, &a.JSON
	rows, cols, settleMs := &a.Rows, &a.Cols, &a.SettleMs
	style, colorOut, withoutSynth := &a.Style, &a.Color, &a.WithoutSynth
	detect, detectAgent := &a.Detect, &a.DetectAgent
	text := a.Text
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
		if *ansi && *asJSON {
			return fmt.Errorf("--ansi cannot be combined with --json (--json encodes the render for a reader that parses; --ansi paints it for one that looks)")
		}
		opts := screenOpts{
			rows: uint16(*rows), cols: uint16(*cols),
			settle:    time.Duration(*settleMs) * time.Millisecond,
			withAttrs: *style, withColor: *colorOut, asJSON: *asJSON, ansi: *ansi,
			includeSynth: !*withoutSynth,
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

// runSessionExecWith is runSessionExec for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionExecWith(cid objproto.ConnectionID, a verb.SessionExecAction) error {
	taskIDHex, cmd := a.TaskID, a.Cmd
	timeout, jsonOut, exitOnly, raw := &a.Timeout, &a.JSON, &a.ExitOnly, &a.Raw

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

// runSessionAwaitIdleWith is runSessionAwaitIdle for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionAwaitIdleWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	taskIDHex := a.TaskID
	thresholdMs, topic := &a.ThresholdMs, &a.Topic
	// The sink is where the fire lands: an operator notification, an
	// agentboard publish, or this call's own long poll.
	sink := protocol.AwaitIdleSink_Reply
	switch {
	case a.Notify:
		sink = protocol.AwaitIdleSink_Notify
	case a.Topic != "":
		sink = protocol.AwaitIdleSink_Board
	}
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.AwaitIdle(ctx, taskIDHex, uint32(*thresholdMs), sink, *topic)
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

// runSessionResizeWith is runSessionResize for a caller that already has the parsed action --
// the generated CLI dispatch.
func runSessionResizeWith(cid objproto.ConnectionID, a verb.SessionAction) error {
	taskIDHex := a.TaskID
	waitMs, quiet := &a.WaitMs, &a.Quiet
	rows, cols, perr := parseResizeSpec(a.Size)
	if perr != nil {
		return perr
	}
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

// parseSession parses one of the single-task session verbs from the
// declaration. The CLI's error mode ends the process with usage, which is what
// a bad command line should do here.
func parseSession(path string, args []string) verb.SessionAction {
	parts := strings.Fields(path)
	sp, ok := verb.Lookup(parts...)
	if !ok {
		die(fmt.Errorf("%s: not in the verb table", path))
	}
	sp = sp.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ExitOnError)
	b, perr := sp.Parse(fs, args)
	if perr != nil {
		die(perr)
	}
	act, berr := sp.BuildFunc()(b)
	if berr != nil {
		fmt.Fprintln(os.Stderr, berr)
		os.Exit(2)
	}
	return act.(verb.SessionAction)
}
