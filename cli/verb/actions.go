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
