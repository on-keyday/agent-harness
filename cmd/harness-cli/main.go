//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// scopeForFlag collects repeatable --scope-for values. It parses and merges on
// every Set, so an overlapping capability list is rejected at the flag rather
// than one round trip later — the server refuses it too, but a typo should not
// cost a spawn.
type scopeForFlag struct{ out []protocol.ScopeOverride }

func (f *scopeForFlag) String() string { return cli.OverridesLabel(f.out) }

func (f *scopeForFlag) Set(v string) error {
	_, ov, err := cli.ParseScopeFor(v)
	if err != nil {
		return err
	}
	merged, err := cli.MergeScopeOverride(f.out, ov)
	if err != nil {
		return err
	}
	f.out = merged
	return nil
}

// workspaceRepo is the `repo` a --workspace supplied, consulted by the two
// places that fall back to HARNESS_REPO_PATH (the file subcommand here and
// `session new` in session.go). A package-level var rather than an os.Setenv:
// the workspace is a tier BELOW the environment, and writing it into the
// environment would erase that distinction for anything reading it later.
var workspaceRepo string

func main() {
	serverCID := flag.String("server-cid", "",
		"server ConnectionID (env: HARNESS_SERVER_CID; workspace: server-cid; default ws:127.0.0.1:8539-*)")
	wsPath := flag.String("ws-path", "", "WebSocket URL path (env: HARNESS_WS_PATH; workspace: ws-path; default /ws)")
	configPath := flag.String("config", "", "workspace config file (env: HARNESS_CONFIG; default ./.harness/config)")
	wsName := flag.String("workspace", "", "workspace whose server-cid / ws-path / repo to use (see `workspace ls`)")
	flag.Usage = usage
	flag.Parse()

	wsFile, _, werr := workspace.Load(*configPath)
	if werr != nil {
		die(fmt.Errorf("config: %w", werr))
	}
	var wsServerCID, wsWSPath string
	if *wsName != "" {
		w, ok := wsFile.Workspace(*wsName)
		if !ok {
			die(fmt.Errorf("config: no workspace named %q", *wsName))
		}
		wsServerCID, wsWSPath, workspaceRepo = w.ServerCID, w.WSPath, w.Repo
	}

	resolvedWS := cliopts.ResolveStringWith(*wsPath, "HARNESS_WS_PATH", wsWSPath)
	if resolvedWS == "" {
		resolvedWS = "/ws"
	}
	cli.WebSocketPath = resolvedWS

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	sub := flag.Arg(0)
	args := flag.Args()[1:]
	ctx := context.Background()

	// The two ladder tiers this SURFACE owns (D7), installed once instead of
	// passed at each call site -- three of which used to pass nil and lose
	// HARNESS_REPO_PATH on verbs that declare it.
	verb.EnvLookup = os.Getenv
	verb.WorkspaceLookup = func(k string) string {
		if k == "repo" {
			return workspaceRepo
		}
		return ""
	}
	workspaceCfgPath = *configPath
	workspaceServerCIDStr = cliopts.ResolveStringWith(*serverCID, "HARNESS_SERVER_CID", wsServerCID)

	parseCID := func() objproto.ConnectionID {
		val := cliopts.ResolveStringWith(*serverCID, "HARNESS_SERVER_CID", wsServerCID)
		if val == "" {
			val = "ws:127.0.0.1:8539-*"
		}
		cid, err := cliopts.ResolveServerCID(val)
		if err != nil {
			die(err)
		}
		return cid
	}

	// Everything the declaration gives this surface, routed by the GENERATED
	// dispatcher. It was a 29-case switch over the FIRST WORD plus, inside
	// eight of those cases, a hand-written walk over the second and third --
	// the shape that let `harness-cli` carry three verbs the table did not
	// know, and let the table carry verbs this binary never reached.
	//
	// `git <task-id> <sub>` is the one line the path match cannot find on its
	// own: the id sits in the MIDDLE of the verb, so no prefix of the tokens
	// is the path. It is peeled here exactly as the TUI and the WebUI peel it,
	// and goes through the same generated entry.
	tokens := append([]string{sub}, args...)
	h := cliVerbs{ctx: ctx, cid: parseCID}
	if sub == "git" {
		if len(args) < 2 {
			die(fmt.Errorf("usage: harness-cli git <task-id> {log|diff|show|status|subrepos|file} ..."))
		}
		act, handled, perr := verb.ParseCLICommand(append([]string{"git", args[1]}, args[2:]...), nil)
		if perr != nil {
			die(perr)
		}
		if !handled {
			die(fmt.Errorf("git: unknown sub-verb %q (log | diff | show | status | subrepos | file)", args[1]))
		}
		g := act.(verb.GitAction)
		g.TaskID = args[0]
		if err, ok := verb.DispatchCLIAction[error](h, g); ok {
			if err != nil {
				die(err)
			}
			return
		}
	}
	if err, handled, perr := verb.DispatchCLILine[error](h, tokens, nil); handled {
		if perr != nil {
			die(perr)
		}
		if err != nil {
			die(err)
		}
		return
	}
	// A family word with no sub-verb -- `file`, `session`, `board` -- reaches
	// here, and so does an outright unknown one. The four with a dedicated
	// usage keep it; every other family gets its sub-verb list FROM THE
	// TABLE, which is where the four hand-written ones (`file
	// {push|pull|ls|mkdir|delete|edit|new}` and its three siblings) each
	// drifted from: `file edit` and `file new` existed for a release without
	// appearing in that line.
	switch {
	case sub == "server":
		serverUsage()
	case sub == "board":
		boardUsage()
	case sub == "agent":
		agentUsage()
	case sub == "workspace":
		workspaceUsage()
	case len(familySubverbs(sub)) > 0:
		fmt.Fprintf(os.Stderr, "usage: harness-cli %s {%s} ...\n",
			sub, strings.Join(familySubverbs(sub), "|"))
	default:
		usage()
	}
	os.Exit(2)
}

// familySubverbs lists the declared second words under one first word, in
// table order and without repeats. Empty when the word heads no multi-word
// path -- which is what tells an unknown verb apart from a family named
// without a sub-verb.
//
// Deduplicated because a three-word path contributes its middle word once per
// leaf: `session stream {attach,turn,approve,interrupt,finish}` would
// otherwise print "stream" five times.
func familySubverbs(head string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range verb.PathsForSurface(verb.CLI) {
		f := strings.Fields(p)
		if len(f) < 2 || f[0] != head || seen[f[1]] {
			continue
		}
		seen[f[1]] = true
		out = append(out, f[1])
	}
	return out
}

// isTaskIDLike reports whether s could be a task id (hex digits only). Used to
// keep `forward ls` / `forward kill` from being mistaken for a task id — neither
// word is hex.
func isTaskIDLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// forwardWConflictsWithLR reports whether -W was combined with -L or -R.
// They are mutually exclusive: -W owns the foreground and exits with its
// peer (like ssh -W, which implies ClearAllForwardings), while -L/-R are
// long-lived listeners started alongside each other.
func forwardWConflictsWithLR(wspec string, nLSpecs, nRSpecs int) bool {
	return wspec != "" && (nLSpecs > 0 || nRSpecs > 0)
}

// interruptStopGrace is how long a first interrupt gets to unwind cleanly
// before the process stops waiting for it.
const interruptStopGrace = 5 * time.Second

// interruptContext cancels ctx on the first interrupt and makes the second one
// authoritative, because a graceful stop that does not finish is
// indistinguishable from a hang at the keyboard.
//
// A forward's clean shutdown depends on the far side: listeners close, control
// streams unwind, registrations deregister. When any of that fails to return —
// which is what "Ctrl-C does not stop it" means — the operator is left with a
// process they cannot end from the terminal. So: the first interrupt cancels,
// and if the process is still alive interruptStopGrace later, or a second
// interrupt arrives, it prints where every goroutine is parked and exits 130.
//
// The dump is the point on Windows, where there is no SIGQUIT to ask for one:
// the stack of a hung shutdown is exactly the evidence needed to fix it, and
// it has to be captured on the machine that reproduces it.
func interruptContext(label string, parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-ctx.Done():
			return
		}
		fmt.Fprintln(os.Stderr, label+": stopping…")
		cancel()
		select {
		case <-stopped:
		case <-sig: // second interrupt: stop waiting
			forceExitWithStacks(label, "interrupted twice")
		case <-time.After(interruptStopGrace):
			forceExitWithStacks(label, fmt.Sprintf("did not stop within %s", interruptStopGrace))
		}
	}()
	return ctx, func() {
		signal.Stop(sig)
		close(stopped)
		cancel()
	}
}

// forceExitWithStacks reports why the process is leaving and dumps every
// goroutine, so a shutdown that hung leaves evidence rather than a mystery.
func forceExitWithStacks(label, reason string) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	fmt.Fprintf(os.Stderr, "%s: %s — forcing exit. Goroutine dump follows.\n%s\n", label, reason, buf[:n])
	os.Exit(130)
}

// stringList collects a repeatable string flag, in the order given — header
// order is preserved all the way to the wire.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ", ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// readFlagBody resolves a --http-body value: a literal, @file, or - for stdin.
func readFlagBody(v string) ([]byte, error) {
	switch {
	case v == "":
		return nil, nil
	case v == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(v, "@"):
		return os.ReadFile(v[1:])
	default:
		return []byte(v), nil
	}
}

func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli [--server-cid CID] [--ws-path PATH] [--config PATH] [--workspace NAME] <subcommand> [args]")
	fmt.Fprintln(w, "")
	// The global flags are not verbs -- they are parsed by the top-level
	// FlagSet before a subcommand is chosen -- so they are the one part of
	// this text the table does not hold.
	fmt.Fprintln(w, "Global flags fall back to env, then to a --workspace (flag > env > workspace > default):")
	fmt.Fprintln(w, "  --server-cid  HARNESS_SERVER_CID  (workspace: server-cid; default ws:127.0.0.1:8539-*)")
	fmt.Fprintln(w, "  --ws-path     HARNESS_WS_PATH     (workspace: ws-path; default /ws)")
	fmt.Fprintln(w, "  --config      HARNESS_CONFIG      (default ./.harness/config; never read inside a task)")
	fmt.Fprintln(w, "  --workspace   NAME                which workspace in that file supplies server-cid / ws-path / repo")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")

	// Every declared CLI verb, in table order, with the SYNOPSIS generated
	// from its flags and positionals. It was 166 hand-written lines, and the
	// completeness test that kept every verb mentioned could not check what
	// the mention SAID: `--caps ... default all` outlived the declaration's
	// flip to default-none, which is the most dangerous direction for a help
	// text to be stale in.
	//
	// Prose that a synopsis cannot carry lives in the table too, as
	// VerbSpec.Notes, so it sits beside the grammar it explains.
	var family string
	for _, path := range verb.PathsForSurface(verb.CLI) {
		sp, ok := verb.Lookup(strings.Fields(path)...)
		if !ok {
			continue
		}
		// Family prose is printed when the family CHANGES, after its last
		// verb, so `git`'s five shared lines appear once instead of six times.
		if head := strings.Fields(path)[0]; head != family {
			printFamilyNotes(w, family)
			family = head
		}
		lines := sp.For(verb.CLI).UsageLines()
		fmt.Fprintf(w, "  %s\n", lines[0])
		for _, n := range lines[1:] {
			fmt.Fprintf(w, "      %s\n", n)
		}
	}
	printFamilyNotes(w, family)

	// The two flags every spawn verb carries, described ONCE and from the
	// declaration: CapsFlagUsage is the same string the --caps flag's own
	// help is built from, and ScopeGrammar is what ParseScope accepts. The
	// old text restated both per verb, which is how "default all" survived
	// the flip to default-none in three places at once.
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Authority (submit / interactive / session new, and `caps set` on a live task):")
	fmt.Fprintf(w, "  --caps   %s\n", verb.CapsFlagUsage)
	fmt.Fprintf(w, "  --scope  which tasks those caps may target: %s (default subtree)\n", cli.ScopeGrammar)
	// Four spaces, not two: at two the reverse guard reads the first word as
	// a verb this binary should accept, which is exactly what that guard is
	// for -- a continuation describes, it does not name.
	fmt.Fprintln(w, "    `caps` prints every capability with the sentence saying what it gates.")
}

// printFamilyNotes writes one family's shared prose, if it declares any.
func printFamilyNotes(w io.Writer, family string) {
	for _, n := range verb.FamilyNotes[family] {
		fmt.Fprintf(w, "      %s\n", n)
	}
}

func serverUsage() { serverUsageTo(os.Stderr) }

func serverUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli server <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  dial-runner [--via RUNNER_CID] RUNNER_CID")
	fmt.Fprintln(w, "                                      ask the server to reverse-dial RUNNER_CID (Phase A/B)")
	fmt.Fprintln(w, "                                      --via relays through an already-connected runner (Phase B)")
	fmt.Fprintln(w, "                                      (runner must be running in --listen / --udp-listen mode)")
	fmt.Fprintln(w, "                                      prints the DialRunnerStatus and exits non-zero on non-Ok")
}

func boardUsage() { boardUsageTo(os.Stderr) }

func boardUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli board <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  topics                              list every topic on the board with metadata (cap: info_global)")
	fmt.Fprintln(w, "  read <topic> [--in-reply-to N] [--json]  print retained messages for <topic> (text: header + pretty payload; --json: JSON Lines, same record shape as agent inbox --json; not found = exit 0)")
	fmt.Fprintln(w, "  subscribers [topic]                 list each task's subscriptions; with <topic>, only the tasks a publish there reaches (cap: info_global)")
	fmt.Fprintln(w, "  retract <topic> --seq N             withdraw one message: gone from every agent path, still readable here")
	fmt.Fprintln(w, "                                      until the topic ages out. --seq is required — there is no whole-topic")
	fmt.Fprintln(w, "                                      retract (cap: purge)")
	fmt.Fprintln(w, "  purge <topic> [--seq N]             drop the whole topic ring (seq=0) or one message by seq. Unlike retract")
	fmt.Fprintln(w, "                                      this destroys the bytes, operator view included (cap: purge)")
}

func agentUsage() { agentUsageTo(os.Stderr) }

func agentUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli agent <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Env-primary (HARNESS_*): SERVER_CID, TASK_ID, RUNNER_ID, HOSTNAME, WS_PATH, REPO_PATH")
	fmt.Fprintln(w, "HARNESS_AUTH_TICKET is env-only (no flag accepted).")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  send --topic T TEXT...              publish a message. The body is the trailing words, or")
	fmt.Fprintln(w, "                                       --data STRING, or --data - to read it from stdin. \"-\" is a")
	fmt.Fprintln(w, "                                       VALUE OF --data, never a positional: `send --topic T -`")
	fmt.Fprintln(w, "                                       publishes the one-byte body \"-\". The ok line reports bytes")
	fmt.Fprintln(w, "                                       and source, so a body that went out wrong says so at once")
	fmt.Fprintln(w, "  send --in-reply-to SEQ TEXT...      reply to SEQ; --topic optional (server routes it where that message asked)")
	fmt.Fprintln(w, "  send --reply-to R ...               route replies to THIS message to R instead of your own")
	fmt.Fprintln(w, "                                       chat.<short-id>; the peer answers with --in-reply-to alone")
	fmt.Fprintln(w, "  wait --topic T [--since N] [--in-reply-to SEQ] [--timeout DUR]")
	fmt.Fprintln(w, "                                       take everything after --since, blocking only if there is nothing;")
	fmt.Fprintln(w, "                                       omitting --since means cursor 0, so a non-empty ring returns AT ONCE")
	fmt.Fprintln(w, "                                       with old messages (scripting; NOT from an agent turn)")
	fmt.Fprintln(w, "  inbox [--since N] [--in-reply-to SEQ]  idempotent dump of subscribed topics; --since 0 (default) = whole ring")
	fmt.Fprintln(w, "  read SEQ                            fetch one retained message, whole; the hooks name it when they")
	fmt.Fprintln(w, "                                       decline to inline a large body. Limited to subscribed topics")
	fmt.Fprintln(w, "  subscribe --topic T                  register a subscription")
	fmt.Fprintln(w, "  unsubscribe --topic T                remove a subscription")
	fmt.Fprintln(w, "  dispatch --topic T TEXT... [--reply-to R] [--timeout DUR]")
	fmt.Fprintln(w, "                                       send, then block for the reply to THAT message. --reply-to R")
	fmt.Fprintln(w, "                                       declares R as the destination AND waits there; default is your")
	fmt.Fprintln(w, "                                       own chat.<short-id>. --timeout bounds the WHOLE call, publish")
	fmt.Fprintln(w, "                                       ack included (scripting; NOT from an agent turn)")
	fmt.Fprintln(w, "  topics                              list every topic on the board (JSON Lines) (cap: info_global)")
	fmt.Fprintln(w, "  subscriptions                       list this agent's registered patterns (JSON Lines)")
	fmt.Fprintln(w, "  retained --topic T | --self          list a topic's retained ring as metadata only, no payload (no cap)")
	fmt.Fprintln(w, "  purge --topic T | --self [--seq N]   drop a topic's retained buffer, or one message by seq (cap: purge)")
	fmt.Fprintln(w, "  retract SEQ                         withdraw a message YOU sent: gone from every agent path, still")
	fmt.Fprintln(w, "                                       visible to the operator as retracted (no cap; authorship-checked)")
	fmt.Fprintln(w, "                                       a reply to a message addressed to you retracts it automatically;")
	fmt.Fprintln(w, "                                       send --no-retire-on-reply to keep one alive past its answer")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// classifyForLocalPrune dials the server, snapshots the task list, and
// returns the subset of taskIDs that are safe to remove locally. A task is
// safe when its server status is terminal (Succeeded/Failed/Cancelled) or
// when it is no longer in the snapshot at all (pruned/typo). Tasks the
// server still considers active (Queued/Running/Detached) are skipped with
// a warning unless force is set.
func classifyForLocalPrune(ctx context.Context, peerCID objproto.ConnectionID, taskIDs []string, force bool, out io.Writer) ([]string, error) {
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	statusByID := make(map[string]protocol.TaskStatus, len(snap.Tasks))
	for i := range snap.Tasks {
		statusByID[hex.EncodeToString(snap.Tasks[i].Id.Id[:])] = snap.Tasks[i].Status
	}
	safe := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		st, known := statusByID[id]
		if !known {
			safe = append(safe, id)
			continue
		}
		switch st {
		case protocol.TaskStatus_Succeeded,
			protocol.TaskStatus_Failed,
			protocol.TaskStatus_Cancelled:
			safe = append(safe, id)
		default:
			if force {
				fmt.Fprintf(out, "force-removing %s (status=%s on server)\n", id, st.String())
				safe = append(safe, id)
			} else {
				fmt.Fprintf(out, "skip %s: still active on server (status=%s); pass --force to override\n", id, st.String())
			}
		}
	}
	return safe, nil
}

// repeatableStrings is a flag.Value that accumulates one entry per occurrence.
// Used for --agent-arg so callers can write
//
//	harness-cli submit --agent-arg --resume --agent-arg <uuid> ...
//
// without shell-quoting concerns. The value is appended in the order the
// flags appear, which is the order forwarded to the agent.
type repeatableStrings []string

func (r *repeatableStrings) String() string {
	if r == nil {
		return ""
	}
	return fmt.Sprint([]string(*r))
}

func (r *repeatableStrings) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// capsExplicitlySet reports whether the "caps" flag was explicitly provided on
// the command line (as opposed to taking its zero-value default). It uses
// flag.FlagSet.Visit which iterates only over flags that were actually set.
func capsExplicitlySet(fs *flag.FlagSet) bool { return flagExplicitlySet(fs, "caps") }

// flagExplicitlySet reports whether the named flag was actually typed. Both
// --caps and --scope need this: their zero values are meaningful ("none" and
// "subtree"), so "was it given?" cannot be read off the value.
func flagExplicitlySet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// runFileEdit pulls a worktree file, opens it in $EDITOR, and writes it back.
// A CLI has no terminal UI of its own to host an editor widget, so unlike the
// TUI this path always goes through an external editor.
func runFileEdit(ctx context.Context, c *cli.Client, taskID, rel string) error {
	doc, err := c.FileEditLoad(ctx, taskID, rel, nil)
	if err != nil {
		return err
	}
	edited, tmp, err := editViaExternalEditor(rel, doc.Text)
	if err != nil {
		return err
	}
	for force := false; ; force = true {
		st, cerr := c.FileEditCommit(ctx, taskID, doc, edited, force)
		if cerr != nil {
			return fmt.Errorf("%w (your edit is kept at %s)", cerr, tmp)
		}
		switch st {
		case cli.FileEditUnchanged:
			os.Remove(tmp)
			fmt.Printf("no change: %s\n", rel)
			return nil
		case cli.FileEditPushed:
			os.Remove(tmp)
			fmt.Printf("saved: %s\n", rel)
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s changed on the runner since it was read. Overwrite? [y/N] ", rel)
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintf(os.Stderr, "not overwritten; your edit is kept at %s\n", tmp)
			return nil
		}
	}
}

// runFileNew opens an empty buffer in $EDITOR and pushes it to rel.
func runFileNew(ctx context.Context, c *cli.Client, taskID, rel string) error {
	text, tmp, err := editViaExternalEditor(rel, "")
	if err != nil {
		return err
	}
	if err := c.FilePushBytes(ctx, taskID, []byte(text), rel, cli.FilePushOpts{MkdirParents: true}, nil); err != nil {
		return fmt.Errorf("%w (your text is kept at %s)", err, tmp)
	}
	os.Remove(tmp)
	fmt.Printf("created: %s\n", rel)
	return nil
}

// editViaExternalEditor spools text to a temp file, runs $EDITOR on it with
// this process's stdio, and returns the result. The temp path comes back too
// so callers can name it when a later step fails and the edit would otherwise
// be lost.
func editViaExternalEditor(name, text string) (string, string, error) {
	f, err := os.CreateTemp("", "harness-edit-*"+filepath.Ext(name))
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", err
	}
	f.Close()
	cmd, err := cli.ExternalEditorCommand(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", tmp, fmt.Errorf("editor exited with an error: %w (your text is kept at %s)", err, tmp)
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		return "", tmp, err
	}
	return string(b), tmp, nil
}

// tapMode turns the four mutually exclusive output flags into one mode. Two at
// once is refused rather than silently ranked: a caller that asked for both
// --raw and --json meant something, and guessing which would be wrong half the
// time.
func tapMode(asHex, asText, asRaw, asJSON bool) (cli.TapRenderMode, error) {
	n := 0
	mode := cli.TapHex
	for _, c := range []struct {
		on bool
		m  cli.TapRenderMode
	}{{asHex, cli.TapHex}, {asText, cli.TapText}, {asRaw, cli.TapRaw}, {asJSON, cli.TapJSON}} {
		if c.on {
			n++
			mode = c.m
		}
	}
	if n > 1 {
		return 0, errors.New("forward tap: --hex, --text, --raw and --json are mutually exclusive")
	}
	return mode, nil
}

// tapModeByName maps the declaration's mode word onto the renderer. The
// mutual exclusion between the four is enforced in the verb's Build, so by the
// time this runs exactly one was chosen.
func tapModeByName(name string) cli.TapRenderMode {
	switch name {
	case "text":
		return cli.TapText
	case "raw":
		return cli.TapRaw
	case "json":
		return cli.TapJSON
	default:
		return cli.TapHex
	}
}

// parseSpawn parses one of the three spawn verbs from the declaration and
// resolves the flags that have a fallback ladder.
//
// The ladder is flag > env > workspace, and it is applied HERE rather than in
// cli/verb because that package parses and does not read the environment or
// the config file.
func parseSpawn(kind string, args []string, _ func() objproto.ConnectionID) verb.SpawnAction {
	path := []string{kind}
	if kind == "session-new" {
		path = []string{"session", "new"}
	}
	sp, ok := verb.Lookup(path...)
	if !ok {
		die(fmt.Errorf("%s: not in the verb table", kind))
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
	a := act.(verb.SpawnAction)
	a.Repo = sp.Resolve(b, "repo", os.Getenv, func(string) string { return workspaceRepo }, nil)
	if a.Repo == "" && a.ResumeTaskID == "" {
		fmt.Fprintf(os.Stderr, "%s: --repo or HARNESS_REPO_PATH required (must match a runner's RepoPath verbatim) — except when --resume is set, which uses the existing task's repo\n", kind)
		os.Exit(2)
	}
	return a
}

// spawnOpts turns the shared action into the client's option bag.
//
// --caps / --scope / --scope-for are already parsed and merged by the verb's
// Build, which is where that grammar lives; the pointers carry "the operator
// said nothing" separately from "the operator said none", because both zero
// values are meaningful.
func spawnOpts(a verb.SpawnAction) cli.SessionOpts {
	var caps protocol.Capability
	if a.Caps != nil {
		caps = *a.Caps
	}
	var scope protocol.TaskScope
	if a.Scope != nil {
		scope = *a.Scope
	}
	sel, err := cli.BuildSelector(cli.SelectorOpts{Runner: a.Runner, Host: a.Host, IP: a.IP})
	if err != nil {
		die(err)
	}
	return cli.SessionOpts{
		Selector: sel, ExtraArgs: a.ExtraArgs, ResumeTaskID: a.ResumeTaskID,
		Caps: caps, Scope: scope, Overrides: a.Overrides,
		ResumeCapsOverride: a.ResumeTaskID != "" && a.CapsPresent,
		ScopePresent:       a.ResumeTaskID != "" && a.ScopePresent,
		ResumeConversation: a.ResumeConversation, AgentProfile: a.Agent,
	}
}
