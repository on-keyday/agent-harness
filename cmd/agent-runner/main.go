package main

import (
	"context"
	"flag"
	"fmt"
	// Aliased: this file already uses `fs` as the local name for a
	// *flag.FlagSet (bindFlags' parameter, main's flag.CommandLine), so a plain
	// `io/fs` import would be shadowed inside exactly the functions most likely
	// to need it next.
	iofs "io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/agentskills"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func resolvePSK(pskVal, pskFile string) []byte {
	if pskVal != "" {
		return []byte(pskVal)
	}
	if pskFile != "" {
		data, err := os.ReadFile(pskFile)
		if err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return []byte(v)
			}
		}
	}
	return nil
}

// mainConfig holds all flag-derived state for agent-runner. Using a struct
// instead of package-level flag vars makes validate() and bindFlags()
// testable without touching the global flag.CommandLine.
type mainConfig struct {
	ServerCID                  string
	Roots                      string
	MaxTasks                   int
	ClaudeBin                  string
	ClaudeArgs                 string
	AgentOneshotArgv           string
	AgentResumeOneshotArgv     string
	AgentInteractiveArgv       string
	AgentResumeInteractiveArgv string
	AgentProfilesJSON          string
	AgentLogFormat             string
	StreamAdapter              string
	WSPath                     string
	Hostname                   string
	PSK                        string
	PSKFile                    string
	NoWorktree                 bool
	ForceInjectHarnessSettings bool
	AgentSkillsDir             string
	Persist                    bool
	NoPersist                  bool
	PingInterval               time.Duration
	ReconnectInit              time.Duration
	ReconnectMax               time.Duration

	// Phase A reverse-dial:
	WSListen  string
	UDPListen string

	// Whether --server-cid was set on the command line (vs default value).
	// Used by validate to distinguish "user set --server-cid AND --listen"
	// (which is an error) from "default --server-cid + --listen" (fine).
	serverCIDExplicit bool

	// Profiles is derived (not flag-bound): built in main() after flag
	// parsing from the default single-agent flags (ClaudeBin/ClaudeArgs/
	// AgentOneshotArgv/...) plus AgentProfilesJSON, then threaded into
	// runCfg.Profiles (runner.Config) below.
	Profiles runner.ProfileSet

	// ShutdownFile, when non-empty, is polled by cli.WatchShutdownFile every
	// 250ms; once the file appears the runner triggers a graceful shutdown.
	// daemon.py injects this automatically when the runner is spawned via
	// scripts/runner.py up, so Windows downs (where SIGTERM can't reach a
	// DETACHED_PROCESS child) can still close the WS cleanly instead of
	// waiting for ping timeout.
	ShutdownFile string
}

// newMainConfig returns a *mainConfig with all flag defaults pre-populated.
func newMainConfig() *mainConfig {
	return &mainConfig{
		ServerCID:     "ws:127.0.0.1:8539-*",
		Roots:         ".",
		MaxTasks:      1,
		ClaudeBin:     "claude",
		WSPath:        "/ws",
		Persist:       true,
		PingInterval:  15 * time.Second,
		ReconnectInit: 500 * time.Millisecond,
		ReconnectMax:  30 * time.Second,
	}
}

// bindFlags registers all flags on fs, using cfg's current values as defaults.
func (c *mainConfig) bindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.ServerCID, "server-cid", c.ServerCID, "server ConnectionID (e.g. ws:host:port-id, * for random); mutually exclusive with --listen/--udp-listen")
	fs.StringVar(&c.Roots, "roots", c.Roots, "comma-separated list of absolute repo root paths this runner serves")
	fs.IntVar(&c.MaxTasks, "max-tasks", c.MaxTasks, "maximum number of concurrent tasks (>= 1)")
	fs.StringVar(&c.ClaudeBin, "agent-bin", c.ClaudeBin, "path to the agent binary")
	fs.StringVar(&c.ClaudeBin, "claude-bin", c.ClaudeBin, "deprecated alias for --agent-bin")
	fs.StringVar(&c.ClaudeArgs, "agent-args", c.ClaudeArgs, "extra args passed to the agent before the task args (whitespace-separated, e.g. \"--dangerously-skip-permissions\")")
	fs.StringVar(&c.ClaudeArgs, "claude-args", c.ClaudeArgs, "deprecated alias for --agent-args")
	fs.StringVar(&c.AgentOneshotArgv, "agent-oneshot-argv", c.AgentOneshotArgv, "argv template for oneshot tasks; tokens {args} and {prompt}; default \"{args} -p {prompt}\"")
	fs.StringVar(&c.AgentResumeOneshotArgv, "agent-resume-oneshot-argv", c.AgentResumeOneshotArgv, "argv template for --resume-conversation oneshot tasks; tokens {args} and {prompt}; default \"{args} --continue -p {prompt}\"")
	fs.StringVar(&c.AgentInteractiveArgv, "agent-interactive-argv", c.AgentInteractiveArgv, "argv template for FRESH interactive opens; token {args}; default \"{args}\" (the bare binary, which is right for claude/codex/agy/a shell) — set it for an agent whose interactive entry point needs a subcommand, e.g. \"chat {args}\"")
	fs.StringVar(&c.AgentResumeInteractiveArgv, "agent-resume-interactive-argv", c.AgentResumeInteractiveArgv, "argv template for --resume-conversation interactive opens; token {args}; default \"{args} --continue\"")
	fs.StringVar(&c.AgentProfilesJSON, "agent-profiles", c.AgentProfilesJSON, "JSON array of extra agent profiles: [{name,bin,oneshotArgv,resumeOneshotArgv,interactiveArgv,resumeInteractiveArgv,agentArgs,logFormat,streamAdapter}]")
	fs.StringVar(&c.AgentLogFormat, "agent-log-format", c.AgentLogFormat, "stdout log decoder for the default agent profile: \"\" (raw), claude-stream-json, or codex-jsonl")
	fs.StringVar(&c.StreamAdapter, "agent-stream-adapter", c.StreamAdapter,
		"path to the event-stream adapter for the default agent profile (e.g. harness-stream-adapter). "+
			"Empty means this profile cannot serve event-stream tasks, and a request for one is refused "+
			"rather than falling back to a PTY. Read per task, so editing the adapter needs no restart")
	fs.StringVar(&c.WSPath, "ws-path", c.WSPath, "WebSocket URL path (overrides cli.WebSocketPath)")
	fs.StringVar(&c.Hostname, "hostname", c.Hostname, "hostname to report in Hello (default: os.Hostname())")
	fs.StringVar(&c.PSK, "psk", c.PSK, "PSK passphrase (env: HARNESS_PSK)")
	fs.StringVar(&c.PSKFile, "psk-file", c.PSKFile, "path to PSK file (env: HARNESS_PSK_FILE)")
	fs.BoolVar(&c.NoWorktree, "no-worktree", c.NoWorktree, "skip per-task git worktree creation; run agent processes directly in the bound repo path. Disables .claude/settings.json and .claude/skills/ injection by default (see --force-inject-harness-settings).")
	fs.BoolVar(&c.ForceInjectHarnessSettings, "force-inject-harness-settings", c.ForceInjectHarnessSettings, "only meaningful with --no-worktree: re-enable .claude/settings.json and .claude/skills/ injection at the bound repo path.")
	fs.StringVar(&c.AgentSkillsDir, "agentskills-dir", c.AgentSkillsDir, "hot-reload: inject agent skills from this directory on disk instead of the embedded copy (env: HARNESS_AGENTSKILLS_DIR). Point it at the repo's runner/agentskills dir; then editing a SKILL.md reaches the NEXT task with no runner rebuild or restart. A subdirectory counts as a skill only if it holds a SKILL.md, so non-skill files in the directory are ignored. Empty (default) injects the embedded copy, which is frozen at this process's launch.")
	fs.BoolVar(&c.Persist, "persist", c.Persist, "auto-reconnect on disconnect (set --no-persist to disable)")
	fs.BoolVar(&c.NoPersist, "no-persist", c.NoPersist, "shortcut for --persist=false")
	fs.DurationVar(&c.PingInterval, "ping-interval", c.PingInterval, "underlying ping cadence; also bounds disconnect detection delay")
	fs.DurationVar(&c.ReconnectInit, "reconnect-initial", c.ReconnectInit, "first backoff after a disconnect")
	fs.DurationVar(&c.ReconnectMax, "reconnect-max", c.ReconnectMax, "backoff cap")
	fs.StringVar(&c.ShutdownFile, "shutdown-file", c.ShutdownFile, "path to a sentinel file the runner polls every 250ms; when it appears the runner triggers a graceful shutdown. daemon.py injects this automatically when the runner is spawned via scripts/runner.py up, so Windows downs (where SIGTERM can't reach a DETACHED_PROCESS child) can still close the WS cleanly instead of waiting for ping timeout.")
	fs.StringVar(&c.WSListen, "listen", c.WSListen, "WebSocket listen host:port for server-initiated reverse-dial mode (mutually exclusive with --server-cid; mirrors harness-server's --listen)")
	fs.StringVar(&c.UDPListen, "udp-listen", c.UDPListen, "UDP listen host:port for server-initiated reverse-dial mode (mutually exclusive with --server-cid). Combine with --listen for ws+udp dualstack.")
}

// isListenMode reports whether either --listen or --udp-listen was set.
func (c *mainConfig) isListenMode() bool {
	return strings.TrimSpace(c.WSListen) != "" || strings.TrimSpace(c.UDPListen) != ""
}

// validate checks mutual-exclusion and required-one-of rules.
func (c *mainConfig) validate() error {
	if c.isListenMode() && c.serverCIDExplicit {
		return fmt.Errorf("--server-cid and --listen/--udp-listen are mutually exclusive")
	}
	if !c.isListenMode() && strings.TrimSpace(c.ServerCID) == "" {
		return fmt.Errorf("must provide either --server-cid (dial mode) or --listen/--udp-listen (reverse-dial mode)")
	}
	if c.MaxTasks < 1 {
		return fmt.Errorf("--max-tasks must be >= 1, got %d", c.MaxTasks)
	}
	if strings.TrimSpace(c.AgentOneshotArgv) != "" && strings.TrimSpace(c.AgentResumeOneshotArgv) == "" {
		return fmt.Errorf("--agent-resume-oneshot-argv is required when --agent-oneshot-argv is customized")
	}
	// Same pairing rule, same reason: an agent whose fresh interactive open
	// needs a subcommand ("chat {args}") almost certainly needs it on the
	// resume open too, and the resume DEFAULT is "{args} --continue" — which
	// would drop the subcommand and launch something else entirely. Requiring
	// both makes that impossible to get half-right.
	if strings.TrimSpace(c.AgentInteractiveArgv) != "" && strings.TrimSpace(c.AgentResumeInteractiveArgv) == "" {
		return fmt.Errorf("--agent-resume-interactive-argv is required when --agent-interactive-argv is customized")
	}
	return nil
}

// resolveAgentSkillsDir applies the --agentskills-dir / HARNESS_AGENTSKILLS_DIR
// precedence (flag wins) and returns the on-disk skill source plus the skill
// names it holds. An empty resolution returns (nil, nil, nil) — the caller
// leaves runner.Config.AgentSkillsFS nil and the embedded copy is used.
//
// A non-empty dir that yields zero skills is an ERROR rather than a silent
// fallback. Skill injection is a best-effort step in the task path (its failure
// is only Warn-logged, see session.go handleAssign), so a typo'd path would
// otherwise produce tasks with no skills at all and nothing louder than one
// warning per task. Failing at startup puts the mistake where the operator is
// still looking.
func resolveAgentSkillsDir(flagVal, envVal string) (iofs.FS, []string, error) {
	dir := strings.TrimSpace(flagVal)
	if dir == "" {
		dir = strings.TrimSpace(envVal)
	}
	if dir == "" {
		return nil, nil, nil
	}
	// Stat first: os.DirFS does not check the path, so a bad one surfaces from
	// ListFS as `open .: no such file or directory` — the "." is the root of the
	// DirFS, and reads as a bug in the runner rather than a wrong flag value.
	st, err := os.Stat(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("--agentskills-dir %q: %w", dir, err)
	}
	if !st.IsDir() {
		return nil, nil, fmt.Errorf("--agentskills-dir %q is not a directory", dir)
	}
	fsys := os.DirFS(dir)
	names, err := agentskills.ListFS(fsys)
	if err != nil {
		return nil, nil, fmt.Errorf("--agentskills-dir %q: %w", dir, err)
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("--agentskills-dir %q holds no skills (a skill is a subdirectory containing SKILL.md)", dir)
	}
	return fsys, names, nil
}

func parseAgentArgsFlag(name, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	args, err := shlex.Split(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return args, nil
}

func main() {
	fs := flag.CommandLine
	cfg := newMainConfig()
	cfg.bindFlags(fs)
	flag.Parse()

	// Detect whether --server-cid was explicitly set on the command line
	// (as opposed to retaining its default value). fs.Visit only visits
	// flags that were explicitly set.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "server-cid" {
			cfg.serverCIDExplicit = true
		}
	})

	if err := cfg.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}

	cli.WebSocketPath = cfg.WSPath

	rawRoots := strings.Split(cfg.Roots, ",")
	var abs []string
	for _, r := range rawRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		a, err := filepath.Abs(r)
		if err != nil {
			slog.Error("roots abs", "root", r, "err", err)
			os.Exit(1)
		}
		// Wire is POSIX '/'-paths. Linux: ToSlash is no-op. Windows: converts
		// '\' separators (and lower-cased drive letters survive as-is). The
		// server treats wire paths as opaque POSIX strings (path package, not
		// path/filepath), so any OS-mismatch between server and runner stays
		// inside the runner binary.
		abs = append(abs, filepath.ToSlash(filepath.Clean(a)))
	}
	if len(abs) < 1 {
		fmt.Fprintf(os.Stderr, "agent-runner: --roots must contain at least one non-empty path\n")
		os.Exit(1)
	}
	oneshotArgv, err := parseAgentArgsFlag("--agent-oneshot-argv", cfg.AgentOneshotArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}
	if err := runner.ValidateOneshotArgvTemplate(oneshotArgv); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: --agent-oneshot-argv: %v\n", err)
		os.Exit(1)
	}
	resumeOneshotArgv, err := parseAgentArgsFlag("--agent-resume-oneshot-argv", cfg.AgentResumeOneshotArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}
	if err := runner.ValidateOneshotArgvTemplate(resumeOneshotArgv); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: --agent-resume-oneshot-argv: %v\n", err)
		os.Exit(1)
	}
	interactiveArgv, err := parseAgentArgsFlag("--agent-interactive-argv", cfg.AgentInteractiveArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}
	if err := runner.ValidateInteractiveArgvTemplate(interactiveArgv); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: --agent-interactive-argv: %v\n", err)
		os.Exit(1)
	}
	resumeInteractiveArgv, err := parseAgentArgsFlag("--agent-resume-interactive-argv", cfg.AgentResumeInteractiveArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}
	if err := runner.ValidateResumeInteractiveArgvTemplate(resumeInteractiveArgv); err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: --agent-resume-interactive-argv: %v\n", err)
		os.Exit(1)
	}

	// The single-agent flags above (--agent-bin/--agent-args/--agent-*-argv)
	// define the default (first) agent profile. --agent-profiles adds extra
	// profiles appended after it. Default profile name = basename of
	// --agent-bin, minus any Windows executable extension so
	// `--agent-bin C:/.../claude.exe` and `--agent-bin claude` advertise the
	// same profile name across the mixed-OS fleet.
	defaultProfile := runner.AgentProfile{
		Name:                  protocol.NormalizeAgentProfileName(filepath.Base(cfg.ClaudeBin)),
		Bin:                   cfg.ClaudeBin,
		AgentArgs:             strings.Fields(cfg.ClaudeArgs),
		OneshotArgv:           oneshotArgv,
		ResumeOneshotArgv:     resumeOneshotArgv,
		InteractiveArgv:       interactiveArgv,
		ResumeInteractiveArgv: resumeInteractiveArgv,
		LogFormat:             cfg.AgentLogFormat,
		StreamAdapter:         cfg.StreamAdapter,
	}
	extraProfiles, err := runner.ParseAgentProfilesJSON(cfg.AgentProfilesJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: --agent-profiles: %v\n", err)
		os.Exit(1)
	}
	profiles, err := runner.NewProfileSet(defaultProfile, extraProfiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: --agent-profiles: %v\n", err)
		os.Exit(1)
	}
	if bad := profiles.UnrecognisedLogFormats(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "agent-runner: unrecognised --agent-log-format/logFormat (falling back to raw output) for %v; recognised: %s\n",
			bad, strings.Join(agentlog.KnownFormats(), ", "))
	}
	if bad := profiles.UnresolvableStreamAdapters(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "agent-runner: event-stream adapter not resolvable now (event-stream tasks on these profiles will fail until it appears; `make build` writes it) for %v\n", bad)
	}
	if bad := profiles.ResolveBinPaths(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "agent-runner: agent bin not resolvable now (kept verbatim; spawn will fail unless it appears on PATH) for %v\n", bad)
	}
	cfg.Profiles = profiles

	hostname := cfg.Hostname
	if hostname == "" {
		nativeHostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		} else {
			hostname = nativeHostname
		}
	}

	// Catch SIGTERM in addition to SIGINT so daemon.py's `p.terminate()`
	// (the default Linux down path; runner.py / runner.sh both route
	// through it) triggers a clean WS Close on shutdown instead of the
	// Go default-kill. Without this the server only notices the runner is
	// gone after the ping interval (~15s) elapses. On Windows
	// syscall.SIGTERM is a no-op — daemon.py uses TerminateProcess for
	// DETACHED_PROCESS children, which is unsignalable from user space;
	// the sentinel-file watcher started below covers that gap.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cli.WatchShutdownFile(ctx, cfg.ShutdownFile, cancel, 250*time.Millisecond, slog.Default())

	pskVal := cfg.PSK
	if pskVal == "" {
		pskVal = os.Getenv("HARNESS_PSK")
	}
	resolvedPSK := resolvePSK(pskVal, cfg.PSKFile)

	// Agent skills: embedded by default; a non-empty --agentskills-dir (or
	// HARNESS_AGENTSKILLS_DIR) swaps in an on-disk FS that WriteAgentSkills
	// re-reads on every task assign. Warn-level like harness-server's
	// --webui-dir, and for the same reason: it bypasses the embedded copy, so
	// what agents are told now depends on a directory rather than on this
	// binary. Logging the resolved skill names makes a half-populated
	// directory visible immediately instead of at the next task.
	agentSkillsFS, agentSkillNames, err := resolveAgentSkillsDir(cfg.AgentSkillsDir, os.Getenv("HARNESS_AGENTSKILLS_DIR"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}
	if agentSkillsFS != nil {
		slog.Warn("agent skills hot-reload: injecting from disk, embedded copy bypassed",
			"skills", agentSkillNames)
	}

	runCfg := runner.Config{
		AllowedRoots:               abs,
		MaxTasks:                   cfg.MaxTasks,
		Hostname:                   hostname,
		Profiles:                   cfg.Profiles,
		Logger:                     slog.Default(),
		PSK:                        resolvedPSK,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
		AgentSkillsFS:              agentSkillsFS,
		PingInterval:               cfg.PingInterval,
	}

	if cfg.isListenMode() {
		// Reverse-dial mode: server connects inbound to the runner.
		// runCfg.ServerCID is intentionally left as the zero ConnectionID here
		// — the listen branch never parses cfg.ServerCID into an objproto.ConnectionID
		// because the runner doesn't know the server's CID at startup.
		// driveAfterConn populates session.ServerCID from the accepted peer.Conn's
		// ConnectionID once the server dials in, so HARNESS_SERVER_CID injected into
		// agent subprocesses points to the actual server endpoint.
		lcfg := runner.ListenConfig{
			Config:    runCfg,
			WSListen:  cfg.WSListen,
			UDPListen: cfg.UDPListen,
			WSPath:    cfg.WSPath,
		}
		if err := runner.ListenAndServe(ctx, lcfg); err != nil && err != context.Canceled {
			slog.Error("runner listen exit", "err", err)
			os.Exit(1)
		}
		return
	}

	// Dial mode (legacy): parse --server-cid and connect outbound.
	peerCID, err := objproto.ParseConnectionID(cfg.ServerCID,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		slog.Error("server-cid", "err", err)
		os.Exit(1)
	}
	runCfg.ServerCID = peerCID

	enabled := cfg.Persist && !cfg.NoPersist

	err = cli.PersistLoop(ctx,
		func(dialCtx context.Context) (cli.PersistHandle, error) {
			return runner.Connect(dialCtx, runCfg)
		},
		func(runCtx context.Context, h cli.PersistHandle) error {
			rh := h.(*runner.RunHandle)
			return runner.OnConnect(runCtx, rh)
		},
		cli.PersistConfig{
			Enabled:        enabled,
			InitialBackoff: cfg.ReconnectInit,
			MaxBackoff:     cfg.ReconnectMax,
			Logger:         slog.Default(),
			OnState: func(s cli.PersistState) {
				slog.Info("runner persist state",
					"phase", s.Phase, "attempt", s.Attempt,
					"next_retry", s.NextRetry, "err", s.LastError)
			},
		})
	if err != nil {
		slog.Error("runner exit", "err", err)
		os.Exit(1)
	}
}
