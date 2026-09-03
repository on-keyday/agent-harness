package verb

import "time"

// Actions are the typed results of parsing. They live here, above the
// surfaces, because the CLI, the TUI and the WebUI all reach the same
// operation and only differ in how they report it.
//
// Every action embeds ActionMarker. A surface may declare its own actions the
// same way -- tui's screen-state verbs (clear, quit, grid, trsf) stay in tui
// and satisfy Action by embedding this marker, which is why it is exported.

// PruneAction asks the server to forget tasks. With TaskIDs empty the server
// runs in time mode (terminal tasks older than Before); with TaskIDs set it
// considers only those, ignores Before, and skips still-active tasks unless
// Force.
type PruneAction struct {
	ActionMarker
	Before  time.Duration
	TaskIDs []string
	Force   bool
}
