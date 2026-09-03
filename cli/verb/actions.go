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

// FilePushAction copies a local file (or a tree, with Recursive) into a task's
// worktree. LocalSrc is empty on the WebUI, where the bytes come from a file
// picker rather than a path -- see the Arg's SurfaceReason.
type FilePushAction struct {
	ActionMarker
	TaskID    string
	LocalSrc  string
	RemoteDst string
	Recursive bool
	Force     bool
	Parents   bool
}

// FilePullAction copies a worktree file (or a tree) out. LocalDst is empty on
// the WebUI, which downloads rather than writing a path.
type FilePullAction struct {
	ActionMarker
	TaskID    string
	RemoteSrc string
	LocalDst  string
	Recursive bool
	Force     bool
	Offset    uint64
	Length    uint64
}

// FileLsAction lists one directory under a task's worktree.
type FileLsAction struct {
	ActionMarker
	TaskID  string
	RelPath string
}

// FileMkdirAction creates a directory in a task's worktree.
type FileMkdirAction struct {
	ActionMarker
	TaskID  string
	RelPath string
	Parents bool
}

// FileDeleteAction removes a file, or a directory with Recursive.
type FileDeleteAction struct {
	ActionMarker
	TaskID    string
	RelPath   string
	Recursive bool
	Force     bool
}

// FileEditAction opens an existing worktree file for editing.
type FileEditAction struct {
	ActionMarker
	TaskID  string
	RelPath string
}

// FileNewAction creates a worktree file from an empty buffer.
type FileNewAction struct {
	ActionMarker
	TaskID  string
	RelPath string
}

// GitAction is a read-only git query against a task's worktree. Sub names the
// query; which of BaseRev/TargetRev are set follows git's own counting --
// none = unstaged, one = that revision against the working tree, two = commit
// against commit.
type GitAction struct {
	ActionMarker
	TaskID    string
	Sub       string
	BaseRev   string
	TargetRev string
	// Path filters within a repository; Subrepo chooses which repository.
	Path      string
	Subrepo   string
	Staged    bool
	Submodule bool
	Max       uint32
	MaxBytes  uint32
}

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

// ServerDialRunnerAction asks the server to reverse-dial a Listen-mode runner.
type ServerDialRunnerAction struct {
	ActionMarker
	RunnerCID string // e.g. "ws:192.168.3.10:8540-*"
	Via       string // empty = direct dial; non-empty = relay via this CID
}

// SSHGatewayAction serves the ssh front door.
type SSHGatewayAction struct {
	ActionMarker
	Listen         string
	HostKeyPath    string
	AuthorizedKeys string
}

// WorkspaceAction is one of the workspace verbs. Sub names which; the fields
// each one reads differ, which is why the flags carrying them are declared
// per surface rather than all at once.
type WorkspaceAction struct {
	ActionMarker
	Sub  string // save | apply | detach | ls | show | rm
	Name string // "" means the installed workspace, except for save

	// CLI-side save knobs: which task to record and what a first-time block
	// should say about resuming it.
	TaskID string
	Resume string
	Runner string
	Repo   string

	// All is `save --all`: write every live session without opening the
	// picker. TUI-only, because the CLI has no picker to skip.
	All bool
	// Stop is `detach --stop`: also stop what the workspace started. Off by
	// default because detach's job is to stop MANAGING -- an operator who
	// detaches after a reconnect-triggered apply should not lose the tunnels
	// they are working through.
	Stop bool
}

// BoardAction is an operator view of the agentboard. Sub names the query;
// Seq is the message a retract or purge targets.
type BoardAction struct {
	ActionMarker
	Sub       string
	Topic     string
	Seq       uint64
	InReplyTo uint64
	JSON      bool
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

// ListAction lists runners and tasks. Tree orders by the creator link.
type ListAction struct {
	ActionMarker
	JSON bool
	Tree bool
	// Filtered is the WebUI's task-list filter pane: the rows it currently
	// admits, rather than the whole snapshot. Declared for that surface alone,
	// and carried here so the Action says what was asked rather than the page
	// reading the flag behind the Action's back.
	Filtered bool
}

// ConnsAction snapshots live connections, or streams their events.
type ConnsAction struct {
	ActionMarker
	JSON   bool
	Follow bool
}

// CatalogAction is the shape of the read-only catalogs: caps, whoami, version.
type CatalogAction struct {
	ActionMarker
	Sub  string
	JSON bool
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

// PruneLocalAction removes worktrees under <repo>/.harness-worktrees/.
type PruneLocalAction struct {
	ActionMarker
	Repo    string
	Before  time.Duration
	TaskIDs []string
	Force   bool
}

// LogsAction dumps a task's log, optionally following it.
type LogsAction struct {
	ActionMarker
	TaskID string
	Follow bool
}

// SessionAction is the shape of the single-task session verbs that carry only
// a task id and a few knobs: attach, ls, kill, await-idle, snapshot, resize,
// and the stream sub-verbs.
type SessionAction struct {
	ActionMarker
	Sub    string
	TaskID string

	View bool // attach

	ThresholdMs uint // await-idle
	Notify      bool
	Topic       string

	Rows, Cols   uint // snapshot
	SettleMs     uint
	Style, Color bool
	Raw, JSON    bool
	ANSI         bool
	WithoutSynth bool
	Detect       bool
	DetectAgent  string

	Size    string // resize
	WaitMs  uint
	Quiet   bool
	FlushMs uint // stream verbs

	Allow      bool // stream approve
	Deny       bool
	Message    string
	Suggestion string
	RequestID  string
}

// AgentAction is the shape of the agentboard verbs an agent calls from inside
// its own task. ServerCID is env-primary; the ticket is env-only.
type AgentAction struct {
	ActionMarker
	Sub                  string
	ServerCID            string
	Topic                string
	Self                 bool
	Seq                  uint64
	Since                uint64
	InReplyTo            uint64
	JSON                 bool
	UserPromptSubmitHook bool
	Timeout              time.Duration
}

// CancelAction cancels a queued or running task.
type CancelAction struct {
	ActionMarker
	TaskID string
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
