package verb

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Actions are the typed results of parsing. They live here, above the
// surfaces, because the CLI, the TUI and the WebUI all reach the same
// operation and only differ in how they report it.
//
// Every action embeds ActionMarker. A surface may declare its own actions the
// same way -- tui's screen-state verbs (clear, quit, grid, trsf) stay in tui
// and satisfy Action by embedding this marker, which is why it is exported.

// ForwardTapAction streams the bytes crossing one forward. A tap sees only
// what crosses after it opens; nothing is recorded server-side.
type ForwardTapAction struct {
	ActionMarker
	ForwardID      uint64
	Dir            string
	MaxRecordBytes uint32
	Mode           string // hex | text | raw | json
}

// SpawnAction starts a task: submit (queued, one-shot), interactive (a PTY
// attached now) or session new (a detachable PTY). One action for all three
// because they differ in what the surface DOES with the result, not in what
// the operator typed.
type SpawnAction struct {
	ActionMarker
	Kind string // submit | interactive | session-new

	Repo string
	Task string // submit's prompt

	// Runner selection. All three were missing from the TUI's submit before
	// the migration, so a task could not be pinned to a host from there.
	Runner string
	Host   string
	IP     string

	ResumeTaskID       string
	ResumeConversation bool

	// Caps and Scope are parsed here rather than carried as strings: the
	// grammar for both lives in this package (ParseCaps / ParseScope), and a
	// pointer is how "the operator said nothing" stays distinct from "the
	// operator said none" -- their zero values are meaningful.
	Caps  *protocol.Capability
	Scope *protocol.TaskScope
	// Overrides carries --scope-for, parsed and merged on every occurrence so
	// an overlapping capability list is refused at the flag rather than one
	// round trip later.
	Overrides []protocol.ScopeOverride

	Agent string
	// ExtraArgs is --agent-arg, repeatable, appended in the order given.
	// --claude-arg is its deprecated alias and accumulates into the same list.
	ExtraArgs []string

	Detach     bool
	Stream     bool
	X11        bool
	X11Display uint
	Rows       uint
	Cols       uint

	// CapsPresent / ScopePresent record whether the operator actually typed
	// them: their zero values are meaningful ("none" and "subtree"), so a
	// resume must not re-grant on a flag nobody passed.
	CapsPresent  bool
	ScopePresent bool
}

// SetCapsAction re-grants a live task's authority. Operator only.
type SetCapsAction struct {
	ActionMarker
	TaskID       string
	Caps         *protocol.Capability
	Scope        *protocol.TaskScope
	Overrides    []protocol.ScopeOverride
	Cascade      bool
	KeepConns    bool
	CapsPresent  bool
	ScopePresent bool
}

// SetParentAction re-points a live task's parent link -- the edge subtree
// scopes walk. Caps and scope are untouched; `caps set` is the verb for those.
type SetParentAction struct {
	ActionMarker
	TaskID   string
	ParentID string
	None     bool
	Swap     bool
}

// ForwardOpenAction opens one or more port forwards and holds them until the
// process ends. CLI-only: a forward's lifetime is the lifetime of the client
// holding its control stream.
type ForwardOpenAction struct {
	ActionMarker
	TaskID string
	L, R   []string
	W      string

	HTTPMethod  string
	HTTPPath    string
	HTTPBody    string
	HTTPHeaders []string
}

// GridAction selects which sessions a grid shows.
type GridAction struct {
	ActionMarker
	Mode   GridScopeMode
	Anchor string
	IDs    []string
}
