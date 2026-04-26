package cli

// WebSocketPath is the URL path used for WebSocket endpoints across the
// harness components (cli / runner / tui / server). The transport package
// itself does not own a path convention; this var is the canonical
// harness-side default. Override at startup via the --ws-path cmd flag
// (set cli.WebSocketPath = *wsPath in main, before calling cli.Dial /
// runner.Run / server.Run).
//
// Default: "/ws"
var WebSocketPath = "/ws"
