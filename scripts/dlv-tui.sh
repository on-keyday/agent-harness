#!/usr/bin/env bash
# dlv-tui.sh — build harness-tui with debug symbols and launch it under
# dlv in headless mode so VSCode (or any DAP client) can attach to it
# while the TUI runs in *this* terminal (which is what raw-mode + alt-screen
# need). Pairs with the "Attach to dlv :2345" config in .vscode/launch.json.
#
# Workflow:
#   1) run this script in a real terminal:
#        ./scripts/dlv-tui.sh --server=127.0.0.1:8539 --repo=$PWD
#      dlv prints "API server listening at: 127.0.0.1:2345" and the TUI
#      stays suspended (waiting for a debugger to issue Continue).
#   2) in VSCode, F5 → "Attach to dlv :2345".
#   3) hit Continue in VSCode (F5 again) — TUI appears in this terminal.
#   4) reproduce the freeze, then in VSCode pause/inspect goroutines.

set -eu

PORT="${DLV_PORT:-2345}"
DLV="${DLV_BIN:-$HOME/go/bin/dlv}"
BIN="${TUI_BIN:-/tmp/harness-tui-debug}"

if [ ! -x "$DLV" ]; then
    echo "dlv not found at $DLV (override via DLV_BIN env)" >&2
    exit 1
fi

# Disable optimization & inlining so step/locals work properly.
go build -gcflags='all=-N -l' -o "$BIN" ./cmd/harness-tui

echo "[dlv-tui] built $BIN; launching dlv on :$PORT (program waits for Continue)"
exec "$DLV" exec "$BIN" \
    --headless \
    --listen=":$PORT" \
    --api-version=2 \
    --accept-multiclient \
    -- "$@"
