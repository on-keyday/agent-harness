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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	pskVal := *psk
	if pskVal == "" {
		pskVal = os.Getenv("HARNESS_PSK")
	}
	resolvedPSK := resolvePSK(pskVal, *pskFile)

	if err := runner.Run(ctx, runner.Config{
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
	}); err != nil {
		slog.Error("runner exit", "err", err)
		os.Exit(1)
	}
}
