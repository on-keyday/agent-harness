// Package webui exposes the embedded browser UI assets (index.html plus the
// static/ tree containing JS, CSS, and the wasm binary) as a stdlib fs.FS.
//
// The harness-server binary mounts this FS on its internal HTTP mux at "/"
// and "/static/" so the same process serves both the WebSocket transport
// (existing /ws route) and the WebUI assets.
//
// NOTE: //go:embed cannot reach files outside the package directory, so this
// embed lives next to the assets. cmd/harness-server imports this package and
// hands FS straight to server.Config.WebUIFS — the FS root already contains
// index.html and the static/ tree directly, no fs.Sub needed.
//
// The `all:` prefix is required so files starting with `.` or `_` are also
// included; it does NOT change the on-disk requirement: webui/static/main.wasm
// must exist at compile time (produced by `make webui-build`) or the runtime
// fs.Stat check in server.Run reports the missing artifact.
package webui

import "embed"

// FS contains the entire webui/ tree, with index.html at "index.html" and
// the static assets at "static/*", relative to the FS root.
//
//go:embed index.html all:static
var FS embed.FS
