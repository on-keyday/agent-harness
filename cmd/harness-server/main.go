package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
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
	listen                = flag.String("listen", "127.0.0.1:8539", "WebSocket listen host:port (use :8539 to dual-stack on all interfaces; loopback by default; empty disables WS leg, requires --udp-listen)")
	udpListen             = flag.String("udp-listen", "", "UDP listen host:port (empty = disabled). Combine with --listen for ws+udp dualstack.")
	dataDir               = flag.String("data-dir", "./harness-data", "persistent data dir")
	taskRetain            = flag.Duration("task-retain", 0, "auto-prune terminal tasks older than this (0 = keep forever)")
	wsPath                = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
	agentboardRing        = flag.Int("agentboard-ring", 64, "agentboard ring buffer entries per topic")
	agentboardTTL         = flag.Duration("agentboard-ttl", 30*time.Minute, "agentboard topic TTL after last publish")
	agentboardMaxTopics   = flag.Int("agentboard-max-topics", 1024, "agentboard max active topics")
	agentboardMaxPayload  = flag.Int("agentboard-max-payload", 64*1024, "agentboard max payload bytes per message")
	psk                   = flag.String("psk", "", "PSK passphrase (env: HARNESS_PSK; empty = disabled)")
	pskFile               = flag.String("psk-file", "", "path to PSK file; auto-generated on first run if absent")
	ringSize              = flag.Int64("detach-ring-buffer-size", 1<<20, "byte size of per-detached-session scrollback ring buffer (default 1 MiB)")
	idleTimeout           = flag.Duration("detach-idle-timeout", 0, "auto-cancel detached sessions after this idle duration (0 = disabled, default)")
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
	// TerminateProcess there, which is unsignalable from user space.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	resolvedPSKVal := *psk
	if resolvedPSKVal == "" {
		resolvedPSKVal = os.Getenv("HARNESS_PSK")
	}
	pskBytes, err := resolvePSK(resolvedPSKVal, *pskFile)
	if err != nil {
		slog.Error("PSK setup failed", "err", err)
		os.Exit(1)
	}

	s := server.New(server.Config{
		Addr:                 strings.TrimSpace(*listen),
		UDPAddr:              strings.TrimSpace(*udpListen),
		DataDir:              *dataDir,
		TaskRetention:        *taskRetain,
		Logger:               slog.Default(),
		PSK:                  pskBytes,
		WebUIFS:              webui.FS,
		DetachRingBufferSize: *ringSize,
		DetachIdleTimeout:    *idleTimeout,
	})
	board := agentboard.New(agentboard.Config{
		RingN:      *agentboardRing,
		TopicTTL:   *agentboardTTL,
		MaxTopics:  *agentboardMaxTopics,
		MaxPayload: *agentboardMaxPayload,
	})
	defer board.Close()
	s.SetBoard(board)

	if err := s.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
