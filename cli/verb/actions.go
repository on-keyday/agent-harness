package verb

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Actions are the typed results of parsing. They live here, above the
// surfaces, because the CLI, the TUI and the WebUI all reach the same
// operation and only differ in how they report it.
//
// Every action embeds ActionMarker. A surface may declare its own actions the
// same way -- tui's screen-state verbs (clear, quit, grid, trsf) stay in tui
// and satisfy Action by embedding this marker, which is why it is exported.

// ExecRunAction runs one command in a task's worktree as its own process --
// separate stdout and stderr, and the command's exit code becomes the
// caller's. NOT `session exec`, which types into a session's foreground shell.
type ExecRunAction struct {
	ActionMarker
	TaskID string
	Argv   []string
	// Shell hands the words to the RUNNER's shell as one line. Joining is
	// right only here: the operator asked for shell interpretation, so these
	// words were never an argv to preserve.
	Shell bool
	// SshdParent gives the command line a parent process NAMED sshd, for a
	// client that checks its own ancestry by process name. Wired on Windows
	// only, and it needs Shell -- what it renames is the shell.
	SshdParent bool
	// Sub distinguishes the run form from ls / kill: the TUI dispatches the
	// whole family through one action, so folding them here keeps that shape
	// rather than making every consumer learn three types.
	Sub        string // run | ls | kill
	ExecID     uint64
	ExecIDs    []uint64
	TaskFilter string
	JSON       bool
}

// ForwardLsAction lists registered port forwards.
type ForwardLsAction struct {
	ActionMarker
	TaskFilter string
	JSON       bool
}

// ForwardKillAction kills registered forwards by id. ForwardID is the first,
// kept because a TUI row kills exactly one; ForwardIDs is the whole list the
// CLI accepts.
type ForwardKillAction struct {
	ActionMarker
	ForwardID  uint64
	ForwardIDs []uint64
}

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

// SendAction types text into a live session as a co-writer -- no takeover.
type SendAction struct {
	ActionMarker
	TaskID string
	Text   string
	// Enter appends a carriage return, i.e. actually submits. Interp
	// interprets backslash escapes. They are DIFFERENT flags: --enter and -e.
	// Merging them would inject a spurious Enter into a live PTY.
	Enter   bool
	Interp  bool
	Quiet   bool
	FlushMs uint
	Resize  string
	// Snapshot renders the screen after sending, which is what makes a
	// drive loop stateless: send, then look.
	Snapshot     bool
	Rows, Cols   uint
	SettleMs     uint
	Style, Color bool
	JSON, ANSI   bool
	WithoutSynth bool
	Detect       bool
	DetectAgent  string
}

// SessionExecAction runs one shell command line in a session's foreground
// shell and blocks. NOT `exec`, which runs its own process in the worktree.
type SessionExecAction struct {
	ActionMarker
	TaskID   string
	Cmd      string
	Timeout  time.Duration
	JSON     bool
	ExitOnly bool
	Raw      bool
}

// StreamTurnAction sends one user turn to an event-stream session.
type StreamTurnAction struct {
	ActionMarker
	TaskID  string
	Text    string
	FlushMs uint
}

// NotifyAction sends one operator notification.
type NotifyAction struct {
	ActionMarker
	Level string
	Title string
	Text  string
}

// AgentSendAction publishes one agentboard message. The body is the trailing
// words, or --data, or stdin -- resolved by the caller, which owns stdin.
type AgentSendAction struct {
	ActionMarker
	Kind            string // send | dispatch
	Topic           string
	Data            string
	DataSet         bool
	Positional      string
	InReplyTo       uint64
	ReplyTo         string
	NoRetireOnReply bool
	Timeout         time.Duration
	ServerCID       string
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
