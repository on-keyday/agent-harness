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

		"bytes_to_target":       float64(fi.BytesToTarget),
		"bytes_from_target":     float64(fi.BytesFromTarget),
		"conns_total":           float64(fi.ConnsTotal),
		"conns_open":            float64(fi.ConnsOpen),
		"taps":                  float64(fi.Taps),
		"last_activity_unix_ms": float64(fi.LastActivityUnixMs),
		"traffic":               PortForwardTrafficLine(fi),
	}
}
