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
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	agentboardMaxPayload = flag.Int("agentboard-max-payload", 1024*1024, "agentboard max payload bytes per message. Costs scale with it in two places: retention (max-topics x ring x this) and the transient read of an in-flight send (this + 64KiB each). It does NOT set what an agent is handed inline — the inbox hooks stop inlining a body past 64KiB and point at a command that fetches it, so raising this admits larger messages without spending the recipient's context on them.")
	psk                  = flag.String("psk", "", "PSK passphrase (env: HARNESS_PSK; empty = disabled)")
	pskFile              = flag.String("psk-file", "", "path to PSK file; auto-generated on first run if absent")
	operatorPSK          = flag.String("operator-psk", "", "operator-only secret (env: HARNESS_OPERATOR_PSK). Operator surfaces (cli/tui/webui) must prove this via the binder; NEVER inject it into agents. With neither this nor --operator-psk-file set, one is generated into <data-dir>/operator-psk.")
	operatorPSKFile      = flag.String("operator-psk-file", "", "path to operator-psk file; auto-generated on first run if absent")
	permitNoOperatorPSK  = flag.Bool("dangerously-permit-no-operator-psk", false, "run WITHOUT an operator secret. Operator surfaces are then validated against --psk (or nothing, when that is empty too), which every in-task agent also holds, so an agent can drop its ticket, reconnect as kind=Client and hold Capability_All. Off by default: an operator secret is required, generated under --data-dir when none is supplied.")
	ringSize             = flag.Int64("detach-ring-buffer-size", 1<<20, "byte size of per-detached-session scrollback ring buffer (default 1 MiB)")
	idleTimeout          = flag.Duration("detach-idle-timeout", 0, "auto-cancel detached sessions after this idle duration (0 = disabled, default)")
	notifyHook           = flag.String("notify-hook", "", "external command line invoked on each notify request (stdin: JSON; env: HARNESS_NOTIFY_*); whitespace-split into executable + args (no quoting). Fallbacks: env HARNESS_NOTIFY_HOOK, then first non-# line of <data-dir>/notify-hook — write the command there once and it survives restarts. Empty everywhere disables egress.")

	shutdownFile = flag.String("shutdown-file", "", "path to a sentinel file the server polls every 250ms; when it appears the server triggers a graceful shutdown. daemon.py injects this automatically when the server is spawned via scripts/server.py up, so Windows downs (where SIGTERM can't reach a DETACHED_PROCESS child) can still close WS connections cleanly instead of being TerminateProcess'd cold.")

	webuiDir = flag.String("webui-dir", "", "dev hot-reload: serve WebUI assets from this directory on disk instead of the embedded copy (env: HARNESS_WEBUI_DIR). Point it at the repo's webui/ dir; then `make webui-build` + a browser refresh picks up wasm/js/css changes with no server rebuild or restart. Empty (default) serves the embedded assets.")

	pprofListen = flag.String("pprof-listen", "", "serve net/http/pprof on this host:port (empty = off, the default). Turning it on ALSO enables the block and mutex profilers, which ship off because they sample on every blocking event: a CPU profile alone answers \"where does the CPU go\", which is the wrong question for a process sitting at half a core — the other half is waiting, and only the block profile says on what. Dev-only and unauthenticated: /debug/pprof exposes goroutine stacks and lets anyone who can reach it start a profile, so bind it to loopback or to a namespace nothing else can reach.")
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

// operatorPSKFileName is where the operator secret is generated when nothing
// names one: under --data-dir, beside the WAL.
const operatorPSKFileName = "operator-psk"

// resolveOperatorPSK is resolvePSK with the default the connect PSK does not
// have: an operator secret is REQUIRED. Given no value and no file it is
// generated into <data-dir>/operator-psk, so a server started with nothing
// still refuses operator authority to whoever has not read that file. Only
// permitNone (--dangerously-permit-no-operator-psk) lifts that; the gate then
// validates operator surfaces against the connect PSK and server.Run warns.
func resolveOperatorPSK(val, file, dataDir string, permitNone bool) ([]byte, error) {
	if val == "" && file == "" {
		if permitNone {
			return nil, nil
		}
		if dataDir == "" {
			return nil, errors.New("no operator PSK and no --data-dir to generate one into; pass --operator-psk / HARNESS_OPERATOR_PSK / --operator-psk-file, or --dangerously-permit-no-operator-psk")
		}
		// server.Run creates the data dir too, but only after this has run.
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("data-dir for %s: %w", operatorPSKFileName, err)
		}
		file = filepath.Join(dataDir, operatorPSKFileName)
	}
	return resolvePSK(val, file)
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
		resolvedOperatorPSKVal = os.Getenv(cli.OperatorPSKEnv)
	}
	operatorPSKBytes, err := resolveOperatorPSK(resolvedOperatorPSKVal, *operatorPSKFile, *dataDir, *permitNoOperatorPSK)
	if err != nil {
		slog.Error("operator-PSK setup failed", "err", err)
		os.Exit(1)
	}
	if *permitNoOperatorPSK && len(operatorPSKBytes) > 0 {
		slog.Warn("--dangerously-permit-no-operator-psk has no effect: an operator PSK was supplied and is enforced")
	}

	nh, nhSource := server.ResolveNotifyHook(*notifyHook, os.Getenv("HARNESS_NOTIFY_HOOK"), *dataDir)
	if nh != "" {
		slog.Info("notify hook configured", "cmd", nh, "source", nhSource)
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

	// Both rates are set to 1 (record every event) rather than a sampling
	// fraction: this listener only exists when somebody asked for it, and a
	// sampled block profile of a rare-but-long stall reports nothing. The
	// overhead is real and perturbs throughput, so measure the rate you want
	// to quote with the flag OFF and use this to explain it, not to produce it.
	if addr := strings.TrimSpace(*pprofListen); addr != "" {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
		go func() {
			slog.Warn("pprof listening — unauthenticated, dev only", "addr", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				slog.Error("pprof listener exited", "err", err)
			}
		}()
	}

	if err := s.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
