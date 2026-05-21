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

var (
	serverCID  = flag.String("server-cid", "ws:127.0.0.1:8539-*", "server ConnectionID (e.g. ws:host:port-id, * for random)")
	roots      = flag.String("roots", ".", "comma-separated list of absolute repo root paths this runner serves")
	maxTasks   = flag.Int("max-tasks", 1, "maximum number of concurrent tasks (>= 1)")
	claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
	claudeArgs = flag.String("claude-args", "", "extra args passed to claude before -p (whitespace-separated, e.g. \"--dangerously-skip-permissions\")")
	wsPath     = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
	hostName   = flag.String("hostname", "", "hostname to report in Hello (default: os.Hostname())")
	psk        = flag.String("psk", "", "PSK passphrase (env: HARNESS_PSK)")
	pskFile    = flag.String("psk-file", "", "path to PSK file (env: HARNESS_PSK_FILE)")

	noWorktree                 = flag.Bool("no-worktree", false, "skip per-task git worktree creation; run agent processes directly in the bound repo path. Disables .claude/settings.json and .claude/skills/ injection by default (see --force-inject-harness-settings).")
	forceInjectHarnessSettings = flag.Bool("force-inject-harness-settings", false, "only meaningful with --no-worktree: re-enable .claude/settings.json and .claude/skills/ injection at the bound repo path.")

	persist       = flag.Bool("persist", true, "auto-reconnect on disconnect (set --no-persist to disable)")
	noPersist     = flag.Bool("no-persist", false, "shortcut for --persist=false")
	pingInterval  = flag.Duration("ping-interval", 15*time.Second, "underlying ping cadence; also bounds disconnect detection delay")
	reconnectInit = flag.Duration("reconnect-initial", 500*time.Millisecond, "first backoff after a disconnect")
	reconnectMax  = flag.Duration("reconnect-max", 30*time.Second, "backoff cap")
)

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath

	if *maxTasks < 1 {
		fmt.Fprintf(os.Stderr, "agent-runner: --max-tasks must be >= 1, got %d\n", *maxTasks)
		os.Exit(1)
	}

	rawRoots := strings.Split(*roots, ",")
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

	hostname := *hostName
	if hostname == "" {
		nativeHostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		} else {
			hostname = nativeHostname
		}
	}

	peerCID, err := objproto.ParseConnectionID(*serverCID,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		slog.Error("server-cid", "err", err)
		os.Exit(1)
	}

	// Catch SIGTERM in addition to SIGINT so daemon.py's `p.terminate()`
	// (the default Linux down path; runner.py / runner.sh both route
	// through it) triggers a clean WS Close on shutdown instead of the
	// Go default-kill. Without this the server only notices the runner is
	// gone after the ping interval (~15s) elapses. On Windows
	// syscall.SIGTERM is a no-op — daemon.py uses TerminateProcess for
	// DETACHED_PROCESS children, which is unsignalable from user space;
	// the ping-timeout path remains the only fallback there.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pskVal := *psk
	if pskVal == "" {
		pskVal = os.Getenv("HARNESS_PSK")
	}
	resolvedPSK := resolvePSK(pskVal, *pskFile)

	runCfg := runner.Config{
		ServerCID:                  peerCID,
		AllowedRoots:               abs,
		MaxTasks:                   *maxTasks,
		Hostname:                   hostname,
		ClaudeBin:                  *claudeBin,
		ExtraClaudeArgs:            strings.Fields(*claudeArgs),
		Logger:                     slog.Default(),
		PSK:                        resolvedPSK,
		NoWorktree:                 *noWorktree,
		ForceInjectHarnessSettings: *forceInjectHarnessSettings,
		PingInterval:               *pingInterval,
	}

	enabled := *persist && !*noPersist

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
			InitialBackoff: *reconnectInit,
			MaxBackoff:     *reconnectMax,
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
