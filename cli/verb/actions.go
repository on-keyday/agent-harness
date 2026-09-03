package verb

// Actions are the typed results of parsing. They live here, above the
// surfaces, because the CLI, the TUI and the WebUI all reach the same
// operation and only differ in how they report it.
//
// Every action embeds ActionMarker. A surface may declare its own actions the
// same way -- tui's screen-state verbs (clear, quit, grid, trsf) stay in tui
// and satisfy Action by embedding this marker, which is why it is exported.

// GridAction selects which sessions a grid shows.
type GridAction struct {
	ActionMarker
	Mode   GridScopeMode
	Anchor string
	IDs    []string
}
