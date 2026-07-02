package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/agent-harness/webui"
)

var (
	listen               = flag.String("listen", "127.0.0.1:8539", "WebSocket listen host:port (use :8539 to dual-stack on all interfaces; loopback by default; empty disables WS leg, requires --udp-listen)")
	udpListen            = flag.String("udp-listen", "", "UDP listen host:port (empty = disabled). Combine with --listen for ws+udp dualstack.")
	dataDir              = flag.String("data-dir", "./harness-data", "persistent data dir")
	taskRetain           = flag.Duration("task-retain", 0, "auto-prune terminal tasks older than this (0 = keep forever)")
	wsPath               = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
	agentboardRing       = flag.Int("agentboard-ring", 64, "agentboard ring buffer entries per topic")
	agentboardTTL        = flag.Duration("agentboard-ttl", 30*time.Minute, "agentboard topic TTL after last publish")
	agentboardMaxTopics  = flag.Int("agentboard-max-topics", 1024, "agentboard max active topics")
	agentboardMaxPayload = flag.Int("agentboard-max-payload", 64*1024, "agentboard max payload bytes per message")
	psk                  = flag.String("psk", "", "PSK passphrase (env: HARNESS_PSK; empty = disabled)")
	pskFile              = flag.String("psk-file", "", "path to PSK file; auto-generated on first run if absent")
	operatorPSK          = flag.String("operator-psk", "", "operator-only secret (env: HARNESS_OPERATOR_PSK). Operator surfaces (cli/tui/webui) must prove this via the binder; NEVER inject it into agents. Empty = legacy behaviour (operator surfaces validated against --psk) with a startup warning, because then an in-task agent can escalate to operator by dropping its ticket.")
	operatorPSKFile      = flag.String("operator-psk-file", "", "path to operator-psk file; auto-generated on first run if absent")
	ringSize             = flag.Int64("detach-ring-buffer-size", 1<<20, "byte size of per-detached-session scrollback ring buffer (default 1 MiB)")
	idleTimeout          = flag.Duration("detach-idle-timeout", 0, "auto-cancel detached sessions after this idle duration (0 = disabled, default)")
	notifyHook           = flag.String("notify-hook", "", "external command invoked on each notify request (stdin: JSON; env: HARNESS_NOTIFY_*); empty disables egress, fallback env HARNESS_NOTIFY_HOOK")

	shutdownFile = flag.String("shutdown-file", "", "path to a sentinel file the server polls every 250ms; when it appears the server triggers a graceful shutdown. daemon.py injects this automatically when the server is spawned via scripts/server.py up, so Windows downs (where SIGTERM can't reach a DETACHED_PROCESS child) can still close WS connections cleanly instead of being TerminateProcess'd cold.")

	webuiDir = flag.String("webui-dir", "", "dev hot-reload: serve WebUI assets from this directory on disk instead of the embedded copy (env: HARNESS_WEBUI_DIR). Point it at the repo's webui/ dir; then `make webui-build` + a browser refresh picks up wasm/js/css changes with no server rebuild or restart. Empty (default) serves the embedded assets.")
)

func resolvePSK(pskVal, pskFile string) ([]byte, error) {
	// 1. Explicit value wins.
	if pskVal != "" {
		return []byte(pskVal), nil
	}
	// 2. No file requested → no PSK.
	if pskFile == "" {
		return nil, nil
	}
	// 3. Attempt to read existing file.
	data, err := os.ReadFile(pskFile)
	if err == nil {
		v := strings.TrimSpace(string(data))
		if v != "" {
			return []byte(v), nil
		}
		// File exists but is blank — fall through to auto-generate.
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("psk-file read: %w", err)
	}
	// 4. File absent or blank → generate, write, return.
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("psk generate: %w", err)
	}
	encoded := hex.EncodeToString(raw[:])
	if err := os.WriteFile(pskFile, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("psk-file write: %w", err)
	}
	slog.Info("generated PSK", "path", pskFile)
	return []byte(encoded), nil
}

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath
	// Catch SIGTERM in addition to SIGINT so daemon.py's `p.terminate()`
	// (the default Linux down path used by server.sh / server.py) closes
	// active WS connections gracefully instead of being killed by the
	// Go default handler. SIGTERM is a no-op on Windows; daemon.py uses
	// TerminateProcess there, which is unsignalable from user space —
	// the sentinel-file watcher started below covers that gap.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cli.WatchShutdownFile(ctx, *shutdownFile, cancel, 250*time.Millisecond, slog.Default())

	resolvedPSKVal := *psk
	if resolvedPSKVal == "" {
		resolvedPSKVal = os.Getenv("HARNESS_PSK")
	}
	pskBytes, err := resolvePSK(resolvedPSKVal, *pskFile)
	if err != nil {
		slog.Error("PSK setup failed", "err", err)
		os.Exit(1)
	}

	resolvedOperatorPSKVal := *operatorPSK
	if resolvedOperatorPSKVal == "" {
		resolvedOperatorPSKVal = os.Getenv("HARNESS_OPERATOR_PSK")
	}
	operatorPSKBytes, err := resolvePSK(resolvedOperatorPSKVal, *operatorPSKFile)
	if err != nil {
		slog.Error("operator-PSK setup failed", "err", err)
		os.Exit(1)
	}

	nh := strings.TrimSpace(*notifyHook)
	if nh == "" {
		nh = strings.TrimSpace(os.Getenv("HARNESS_NOTIFY_HOOK"))
	}

	// WebUI assets: embedded by default; a non-empty --webui-dir (or
	// HARNESS_WEBUI_DIR) swaps in an on-disk FS so static/* (main.wasm,
	// main.js, css) is read fresh per request — rebuild + browser refresh,
	// no server rebuild. The field is an fs.FS, so the server code is
	// unchanged. Trade-off: this bypasses the integrity/deploy benefit of
	// embedding, so it is a dev-only knob, off by default.
	var webUIFS fs.FS = webui.FS
	resolvedWebUIDir := strings.TrimSpace(*webuiDir)
	if resolvedWebUIDir == "" {
		resolvedWebUIDir = strings.TrimSpace(os.Getenv("HARNESS_WEBUI_DIR"))
	}
	webUINoCache := false
	if resolvedWebUIDir != "" {
		webUIFS = os.DirFS(resolvedWebUIDir)
		webUINoCache = true // hot-reload: defeat browser heuristic caching of js/wasm
		slog.Warn("WebUI hot-reload: serving assets from disk, embedded copy bypassed", "dir", resolvedWebUIDir)
	}

	s := server.New(server.Config{
		Addr:                 strings.TrimSpace(*listen),
		UDPAddr:              strings.TrimSpace(*udpListen),
		DataDir:              *dataDir,
		TaskRetention:        *taskRetain,
		Logger:               slog.Default(),
		PSK:                  pskBytes,
		OperatorPSK:          operatorPSKBytes,
		WebUIFS:              webUIFS,
		WebUINoCache:         webUINoCache,
		DetachRingBufferSize: *ringSize,
		DetachIdleTimeout:    *idleTimeout,
		NotifyHook:           nh,
	})
	board := agentboard.New(agentboard.Config{
		RingN:      *agentboardRing,
		TopicTTL:   *agentboardTTL,
		MaxTopics:  *agentboardMaxTopics,
		MaxPayload: *agentboardMaxPayload,
		// Boot epoch: start the publish seq strictly above any prior boot's
		// range so persisted --since-last cursors stay valid across restarts.
		// (wall-clock ms << 20 leaves ~1M headroom per boot before the next
		// boot's epoch; a restart always advances because time advances.)
		SeqSeed: uint64(time.Now().UnixMilli()) << 20,
	})
	defer board.Close()
	s.SetBoard(board)

	// Debug: SIGUSR1 (Unix) dumps every connection's trsf internal state.
	installTrsfDump(s)

	if err := s.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
