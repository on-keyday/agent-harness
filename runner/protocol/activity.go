package protocol

import "time"

// ActivityBusyThreshold separates "busy" from "idle" for a live interactive
// session's PTY output quiescence. Byte-level basis (measured 2026-07-12
// against claude): an in-flight agent TUI repaints its spinner ~every 100ms
// with a max observed gap of ~0.5s, while an idle prompt emits nothing at all
// — so 3s cleanly separates the two with wide margin.
//
// Shared by BOTH sides of the wire so their notions of busy/idle agree:
//   - server: SessionMux's activity watcher emits task_activity events when
//     output quiescence crosses this edge;
//   - clients: cli.ActivityStr renders the badge from output_idle_ms with the
//     same cut, so an idle-edge event (output_idle_ms >= threshold) never
//     renders as "busy" or vice versa.
//
// The await-idle RPC keeps its own independent server-side default.
const ActivityBusyThreshold = 3 * time.Second
