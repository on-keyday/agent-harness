package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
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
	ServerCID    string
	Roots        string
	MaxTasks     int
	ClaudeBin    string
	ClaudeArgs   string
	WSPath       string
	Hostname     string
	PSK          string
	PSKFile      string
	NoWorktree   bool
	ForceInjectHarnessSettings bool
	Persist      bool
	NoPersist    bool
	PingInterval  time.Duration
	ReconnectInit time.Duration
	ReconnectMax  time.Duration

	// Phase A reverse-dial:
	WSListen  string
	UDPListen string

	// Whether --server-cid was set on the command line (vs default value).
	// Used by validate to distinguish "user set --server-cid AND --listen"
	// (which is an error) from "default --server-cid + --listen" (fine).
	serverCIDExplicit bool

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
	fs.StringVar(&c.ClaudeBin, "claude-bin", c.ClaudeBin, "path to the claude binary")
	fs.StringVar(&c.ClaudeArgs, "claude-args", c.ClaudeArgs, "extra args passed to claude before -p (whitespace-separated, e.g. \"--dangerously-skip-permissions\")")
	fs.StringVar(&c.WSPath, "ws-path", c.WSPath, "WebSocket URL path (overrides cli.WebSocketPath)")
	fs.StringVar(&c.Hostname, "hostname", c.Hostname, "hostname to report in Hello (default: os.Hostname())")
	fs.StringVar(&c.PSK, "psk", c.PSK, "PSK passphrase (env: HARNESS_PSK)")
	fs.StringVar(&c.PSKFile, "psk-file", c.PSKFile, "path to PSK file (env: HARNESS_PSK_FILE)")
	fs.BoolVar(&c.NoWorktree, "no-worktree", c.NoWorktree, "skip per-task git worktree creation; run agent processes directly in the bound repo path. Disables .claude/settings.json and .claude/skills/ injection by default (see --force-inject-harness-settings).")
	fs.BoolVar(&c.ForceInjectHarnessSettings, "force-inject-harness-settings", c.ForceInjectHarnessSettings, "only meaningful with --no-worktree: re-enable .claude/settings.json and .claude/skills/ injection at the bound repo path.")
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
	return nil
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

	runCfg := runner.Config{
		AllowedRoots:               abs,
		MaxTasks:                   cfg.MaxTasks,
		Hostname:                   hostname,
		ClaudeBin:                  cfg.ClaudeBin,
		ExtraClaudeArgs:            strings.Fields(cfg.ClaudeArgs),
		Logger:                     slog.Default(),
		PSK:                        resolvedPSK,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
		PingInterval:               cfg.PingInterval,
	}

	if cfg.isListenMode() {
		// Reverse-dial mode: server connects inbound to the runner.
		// runCfg.ServerCID is intentionally left as the zero ConnectionID here
		// — the listen branch never parses cfg.ServerCID into an objproto.ConnectionID
		// because the runner doesn't know the server's CID at startup. session.ServerCID
		// inherits this zero value, so HARNESS_SERVER_CID in agent subprocesses will be
		// empty / invalid during Phase A. Task 6 will populate ServerCID from the
		// accepted connection's peer identity.
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
