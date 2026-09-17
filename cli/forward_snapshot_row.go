package cli

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ForwardSnapshotRow converts one registration into the object the WebUI's
// forward list renders.
//
// It lives here rather than inside the wasm bridge's snapshot closure for two
// reasons. The wasm package builds only under GOOS=js, so a builder in there
// can be compiled but not tested; and every value below is already produced by
// a cli renderer, so keeping the assembly beside them is what stops the browser
// from growing a second one.
//
// Counters are emitted BOTH raw and rendered. The raw numbers are what a
// consumer computes with — the capsBits/scopeBase pattern, where a label cannot
// be re-parsed — and `traffic` is exactly the line `forward ls` prints, so the
// browser shows the same text without re-deriving the format in JS.
func ForwardSnapshotRow(fi *protocol.PortForwardInfo) map[string]any {
	return map[string]any{
		"forward_id": float64(fi.ForwardId),
		"dir":        PortForwardDirFlag(fi.Direction),
		"task":       hex.EncodeToString(fi.TaskId.Id[:]),
		"spec":       PortForwardSpecString(fi),
		// origin is the single "kind cid" string: the CLI, TUI and WebUI all
		// render this helper's output so the three agree on what "origin"
		// means. The cid half is what distinguishes two identical specs started
		// by different clients.
		"origin": PortForwardOrigin(fi),
		// origin_cid is the join key for the topology's forward edges: it
		// matches conns[].cid exactly. Splitting the display form above to
		// recover it would make the diagram depend on a formatting convention.
		"origin_cid": string(fi.OriginCid),

		// What the row carries and how it is carried. On every row, tcp
		// included: a key that disappeared for tcp would make the browser's
		// rendering decision impossible to write once.
		"protocol": fi.Protocol.String(),
		"route":    fi.Route.String(),

		"bytes_to_target":       float64(fi.BytesToTarget),
		"bytes_from_target":     float64(fi.BytesFromTarget),
		"conns_total":           float64(fi.ConnsTotal),
		"conns_open":            float64(fi.ConnsOpen),
		"taps":                  float64(fi.Taps),
		"last_activity_unix_ms": float64(fi.LastActivityUnixMs),

		// Datagram accounting, raw and by the counter's own name. Its RENDERED
		// form is inside `traffic` below, which already applies the existence
		// gate — so the browser inherits that rule instead of reimplementing it
		// in JS, which is the whole reason this assembly lives beside the
		// renderers. Absent entirely on a row carrying no counters.
		"counters": forwardCountersAny(fi),

		"traffic": PortForwardTrafficLine(fi),
	}
}

// forwardCountersAny is forwardCountersMap in the shape this snapshot uses:
// float64 values, because the browser receives these through JSON and a
// consumer that type-switches on the map's values should not have to handle two
// numeric types depending on which assembly produced it.
func forwardCountersAny(fi *protocol.PortForwardInfo) map[string]any {
	m := make(map[string]any, len(fi.Counters))
	for _, c := range fi.Counters {
		m[c.Key.String()] = float64(c.Value)
	}
	return m
}
