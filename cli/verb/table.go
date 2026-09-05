package verb

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Verbs is the declaration: one entry per verb path, and the single place a
// verb's grammar is written down. The CLI builds its FlagSet from an entry,
// the TUI parses through one, and the WebUI reaches one across the wasm
// bridge -- so a flag added here reaches three surfaces, and a flag added to
// only one surface is a drift the completeness tests refuse.
//
// Migration is by verb family. prune is first because it exercises every
// mechanism exactly once: it is on all three surfaces, is the smallest family,
// has a real alias (--force/-f), takes no free-form trailing text, and is the
// one family whose TUI parser had no arity check at all.
var Verbs = []VerbSpec{
	{
		Path: []string{"prune"},
		Notes: []string{
			"ask the server to forget tasks",
			"no ids: terminal tasks older than --before",
			"with ids: only those (refuses active tasks unless --force)",
		},
		Surfaces: CLI | TUI | WebUI,
		Action:   "PruneAction",
		// The widest form has to be ASKED for. A bare `prune` forgot every
		// terminal task older than the default, and the server deletes the
		// TaskEntry and its log -- after which `submit --resume <id>` answers
		// resume_not_found, because handleSubmitResume's first precondition is
		// that the entry still exists. Naming neither ids nor a cutoff is the
		// `board purge --seq` shape one step earlier: there the value was
		// dropped, here it was never typed.
		AtLeastOne: []Rule{{Flags: []string{"task-id", "before"},
			Reason: "a bare prune forgets every terminal task older than the default; say which"}},
		Args: []Arg{
			// Variadic and optional: no ids means time mode, which is the
			// difference between "forget old terminal tasks" and "forget
			// exactly these".
			{Name: "task-id", Type: ArgTaskID, Variadic: true, Field: "TaskIDs", WidensIfUnset: true},
		},
		Flags: []Flag{
			{
				Name: "before", Type: FlagDuration, Default: 7 * 24 * time.Hour, Field: "Before",
				Help: "forget terminal tasks older than this (ignored when TASK_IDs are passed)",
			},
			{
				Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Force",
				Help: "with TASK_IDs: also forget tasks the server still considers active (Queued/Running/Detached)",
			},
		},
		Examples: []string{
			// No bare `prune` here any more: the widest form is the one that
			// has to be typed out.
			"prune --before 24h",
			"prune --force aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			// The flag AFTER the ids, which is the form stdlib parsing drops
			// and the reason this verb permutes.
			"prune aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --force",
		},
	},
	// --- file ---
	//
	// Seven sub-verbs, and the family where per-surface narrowing first bites:
	// a browser has no local path, so `file push` names two positionals there
	// and three everywhere else. Declared with Arg.Surfaces rather than as a
	// separate verb, because it is one operation reached from three places.
	{
		Path: []string{"file", "push"},
		Notes: []string{
			"copy a local file (or directory tree with -r) into the worktree",
			"default: O_EXCL refuses to overwrite; -f permits replacement",
		},
		Action:   "FilePushAction",
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{
				Name: "local-src", Type: ArgString, Field: "LocalSrc",
				Surfaces:      CLI | TUI,
				SurfaceReason: "a browser has no local path to name; the WebUI supplies the bytes from a file picker",
			},
			{Name: "worktree-rel-dst", Type: ArgString, Field: "RemoteDst"},
		},
		Flags: []Flag{
			{Name: "recursive", Aliases: []string{"r"}, Type: FlagBool, Default: false, Field: "Recursive",
				Help: "transfer a directory tree"},
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Force",
				Help: "overwrite existing destination"},
			{Name: "parents", Aliases: []string{"p"}, Type: FlagBool, Default: false, Field: "Parents",
				Help: "create missing parent directories of the destination (mkdir -p)"},
		},
		Examples: []string{
			"file push aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ./local.txt docs/local.txt",
			"file push -r -f aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ./dir docs/dir",
		},
	},
	{
		Path: []string{"file", "pull"},
		Notes: []string{
			"copy a worktree file (or directory tree with -r) to a local path",
			"default: O_EXCL refuses to overwrite local; -f permits replacement",
		},
		Action: "FilePullAction",
		// A directory pull is a generated tar, whose byte offsets are not a
		// stable thing to index into.
		Exclusive: []Rule{{Flags: []string{"recursive", "offset"}}, {Flags: []string{"recursive", "length"}}},
		Surfaces:  CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "worktree-rel-src", Type: ArgString, Field: "RemoteSrc"},
			{
				Name: "local-dst", Type: ArgString, Field: "LocalDst",
				Surfaces:      CLI | TUI,
				SurfaceReason: "a browser downloads the file rather than writing it to a path it names",
			},
		},
		Flags: []Flag{
			{Name: "recursive", Aliases: []string{"r"}, Type: FlagBool, Default: false, Field: "Recursive",
				Help: "transfer a directory tree"},
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Force",
				Surfaces:      CLI | TUI,
				SurfaceReason: "a browser hands the bytes to its save dialog; there is no destination here to overwrite",
				Help:          "overwrite existing destination"},
			// -o / -n existed only in the TUI before the migration. Adding them
			// to the other surfaces widens what parses and never narrows it.
			{Name: "offset", Aliases: []string{"o"}, Type: FlagUint64, Default: uint64(0), Field: "Offset",
				Help: "first byte to pull (single-file pull only)"},
			{Name: "length", Aliases: []string{"n"}, Type: FlagUint64, Default: uint64(0), Field: "Length",
				Help: "max bytes to pull; 0 = to end of file"},
		},
		Examples: []string{
			"file pull aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/x.txt ./x.txt",
			"file pull -r aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs ./docs",
		},
	},
	{
		Path: []string{"file", "ls"},
		Notes: []string{
			"list a single directory under the worktree (default: worktree root)",
		},
		Action:   "FileLsAction",
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			// Optional, not a list: MaxCount 1 makes it a single value the
			// generator writes as a string rather than a slice nobody indexes
			// past [0].
			{Name: "worktree-rel-dir", Type: ArgString, Variadic: true, MaxCount: 1, Field: "RelPath"},
		},
		Examples: []string{
			"file ls aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"file ls aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs",
		},
	},
	{
		Path: []string{"file", "mkdir"},
		Notes: []string{
			"create a directory in the worktree",
		},
		Action:   "FileMkdirAction",
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "worktree-rel-dir", Type: ArgString, Field: "RelPath"},
		},
		Flags: []Flag{
			{Name: "parents", Aliases: []string{"p"}, Type: FlagBool, Default: false, Field: "Parents",
				Help: "create missing parent directories (mkdir -p); also makes an existing directory a success"},
		},
		Examples: []string{"file mkdir -p aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/sub"},
	},
	{
		Path: []string{"file", "delete"},
		Notes: []string{
			"remove a file; -r a directory (dir_delete), -r -f a non-empty directory (RemoveAll); without -r a directory is refused",
		},
		Action:   "FileDeleteAction",
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "worktree-rel-path", Type: ArgString, Field: "RelPath"},
		},
		Flags: []Flag{
			{Name: "recursive", Aliases: []string{"r"}, Type: FlagBool, Default: false, Field: "Recursive",
				Help: "target a directory tree instead of a single file (uses dir_delete)"},
			// Without -r this flag is ignored, so its absence never widens: -r
			// alone refuses a non-empty directory rather than emptying it.
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Force",
				Help: "with -r: delete non-empty directory contents recursively (RemoveAll). Ignored without -r"},
		},
		Examples: []string{
			"file delete aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/x.txt",
			"file delete -r -f aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs",
		},
	},
	{
		Path: []string{"file", "edit"},
		Notes: []string{
			"open the file in $EDITOR and write it back",
		},
		Action:   "FileEditAction",
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "worktree-rel-path", Type: ArgString, Field: "RelPath"},
		},
		Examples: []string{"file edit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/x.txt"},
	},
	{
		Path: []string{"file", "new"},
		Notes: []string{
			"create an empty file (refused when it exists)",
		},
		Action:   "FileNewAction",
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "worktree-rel-path", Type: ArgString, Field: "RelPath"},
		},
		Examples: []string{"file new aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/new.txt"},
	},
	// --- git ---
	//
	// `git <sub> <task-id>`, the same shape as `file ls <task-id>`. It used to
	// be `git <task-id> <sub>` -- the only family in the table whose id came
	// before its sub-verb -- which meant no prefix of the line was the path,
	// so all three surfaces peeled the id by hand against a literal "git" and
	// each carried its own copy of the sub-verb list to report a bad one. The
	// design doc that fixed the order gave no reason for it, while the same
	// doc's stated principle is that a hand which knows git should not have to
	// learn a new noun -- and real git puts the sub-verb first.
	//
	// Sub-verbs differ in how many revisions they take, which is why each is
	// its own entry rather than one `git` verb with a mode flag.
	//
	// --max-bytes was missing from the TUI before the migration, so a large
	// diff could not be capped there; declaring it once gives it to every
	// surface. The WebUI accepted it and threw it away, which was worse than
	// refusing it.
	{
		Path:          []string{"git", "log"},
		Surfaces:      CLI | TUI | WebUI,
		Pathspec:      true,
		PathspecField: "Path",
		Action:        "GitAction",
		Const:         map[string]string{"Sub": "log"},
		Args:          []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}, {Name: "revision", Type: ArgString, Variadic: true, MaxCount: 1, Field: "BaseRev"}},
		Flags: []Flag{
			{Name: "max", Type: FlagUint, Default: uint(0), Field: "Max", FieldType: "uint32", Help: "maximum commits (0 = 100, capped at 1000)"},
			{Name: "subrepo", Type: FlagString, Default: "", Field: "Subrepo", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git log aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "git log aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --max 20"},
	},
	{
		Path: []string{"git", "diff"},
		Notes: []string{
			"counts revisions the way git does: none = unstaged, one = that revision",
			"against the working tree, two = commit against commit",
			"--submodule inlines a submodule's own changes",
		},
		Surfaces:      CLI | TUI | WebUI,
		Pathspec:      true,
		PathspecField: "Path",
		Action:        "GitAction",
		Const:         map[string]string{"Sub": "diff"},
		ExtraFields:   map[string]string{"TaskID": "string"},
		// Counted the way git counts them: none = unstaged, one = that revision
		// against the working tree, two = commit against commit. Two Optional
		// positionals rather than a slice a Build interprets, so the mapping
		// is generated like every other verb's.
		Args: []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "base", Type: ArgString, Optional: true, Field: "BaseRev"},
			{Name: "target", Type: ArgString, Optional: true, Field: "TargetRev"},
		},
		Flags: []Flag{
			// A long-to-long alias: --cached is git's own spelling of --staged,
			// bound to one variable, unlike session send's --enter and -e.
			{Name: "staged", Aliases: []string{"cached"}, Type: FlagBool, Default: false, Field: "Staged",
				Help: "diff the index instead of the working tree"},
			{Name: "submodule", Type: FlagBool, Default: false, Field: "Submodule",
				Help: "inline a submodule's own file-level changes (the output is then not an applyable patch)"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Field: "MaxBytes", FieldType: "uint32", Help: "maximum diff bytes (0 = 2MiB, capped at 8MiB)"},
			{Name: "subrepo", Type: FlagString, Default: "", Field: "Subrepo", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git diff aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "git diff aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --staged", "git diff aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa HEAD~1 HEAD"},
		// The revisions are counted the way git counts them -- none = unstaged,
		// one = that revision against the working tree, two = commit against
		// commit -- which is an interpretation of the positionals, not a
		// mapping, so this one keeps a Build. The arity cap and the --staged
		// conflict are declared.
		Validate: func(b Bound) error {
			// Args[0] is the task id, so the revisions are what follows it.
			// Named rather than counted from the whole slice: the count was
			// written against a grammar where the id was not a positional at
			// all, and every such check went off by one when it became one.
			if len(b.Args) > 0 && len(b.Args[1:]) == 2 && b.Bool("staged") {
				return fmt.Errorf("git diff: --staged names the index as the right-hand side, so a second revision has nowhere to go")
			}
			return nil
		},
	},
	{
		Path: []string{"git", "show"},
		Notes: []string{
			"--submodule inlines a submodule's own changes",
		},
		Surfaces:      CLI | TUI | WebUI,
		Pathspec:      true,
		PathspecField: "Path",
		Action:        "GitAction",
		Const:         map[string]string{"Sub": "show"},
		Args:          []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}, {Name: "revision", Type: ArgString, Variadic: true, MaxCount: 1, Field: "BaseRev"}},
		Flags: []Flag{
			{Name: "submodule", Type: FlagBool, Default: false, Field: "Submodule", Help: "inline a submodule's own file-level changes"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Field: "MaxBytes", FieldType: "uint32", Help: "maximum bytes (0 = 2MiB, capped at 8MiB)"},
			{Name: "subrepo", Type: FlagString, Default: "", Field: "Subrepo", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git show aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "git show aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa HEAD"},
	},
	{
		Path:          []string{"git", "status"},
		Args:          []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Surfaces:      CLI | TUI | WebUI,
		Pathspec:      true,
		PathspecField: "Path",
		Action:        "GitAction",
		Const:         map[string]string{"Sub": "status"},
		Flags: []Flag{
			{Name: "subrepo", Type: FlagString, Default: "", Field: "Subrepo", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git status aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"git", "subrepos"},
		Args: []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Notes: []string{
			"list the nested repositories under the worktree",
		},
		Surfaces:      CLI | TUI | WebUI,
		Pathspec:      true,
		PathspecField: "Path",
		Action:        "GitAction",
		Const:         map[string]string{"Sub": "subrepos"},
		Flags: []Flag{
			{Name: "subrepo", Type: FlagString, Default: "", Field: "Subrepo", Help: "list nested repos under this worktree-relative directory"},
		},
		Examples: []string{"git subrepos aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path:          []string{"git", "file"},
		Surfaces:      CLI | TUI | WebUI,
		Pathspec:      true,
		PathspecField: "Path",
		Action:        "GitAction",
		Const:         map[string]string{"Sub": "file"},
		// The path may arrive as a positional OR after `--`, never both and
		// never neither -- so one lifted out of a diff header works either
		// way. Neither an arity nor an exclusion: it is a choice between two
		// DIFFERENT carriers, which no attribute expresses.
		Validate: func(b Bound) error {
			// Args[0] is the task id; the path is what may follow it.
			paths := b.Args
			if len(paths) > 0 {
				paths = paths[1:]
			}
			switch {
			case len(paths) == 1 && b.Pathspec != "":
				return fmt.Errorf("git file: path given twice — once as an argument and once after `--`")
			case len(paths) == 0 && b.Pathspec == "":
				return fmt.Errorf("git file: a path is required, as an argument or after `--`")
			}
			return nil
		},
		// Both carriers write the same field, and Validate above refuses the
		// two cases where that would be ambiguous. The pathspec assignment
		// runs after the positional one, so `-- <path>` wins when it is the
		// only one given -- and it cannot be given alongside the other.
		Args: []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}, {Name: "path", Type: ArgString, Optional: true, Field: "Path"}},
		Flags: []Flag{
			{Name: "staged", Type: FlagBool, Default: false, Field: "Staged", Help: "read the indexed copy"},
			{Name: "rev", Type: FlagString, Default: "", Field: "TargetRev", Help: "read the copy at this revision"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Field: "MaxBytes", FieldType: "uint32", Help: "maximum bytes (0 = 2MiB, capped at 8MiB)"},
			{Name: "subrepo", Type: FlagString, Default: "", Field: "Subrepo", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git file aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa README.md", "git file aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --rev HEAD~1 README.md"},
	},
	// --- exec (exec_run) ---
	{
		Path: []string{"exec"},
		Notes: []string{
			"run a command in the task's WORKTREE as its own process:",
			"stdout and stderr stay separate, and the command's own exit code becomes ours",
			"works on a FINISHED task too, as long as its worktree is still there \u2014",
			"a task that ended with uncommitted work keeps one",
			"dies with this process; for something to leave running, submit a task instead",
			"NOT `session exec`, which types into the session's foreground shell",
			"--shell: hand it to the RUNNER's shell as one line (sh -c / cmd /c by its platform)",
		},
		Surfaces: CLI | TUI | WebUI,
		Action:   "ExecRunAction",
		Const:    map[string]string{"Sub": "run"},
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		// The argv follows a literal `--` and stays a LIST: the runner needs
		// the word boundaries. --shell is the one case that joins, and it does
		// so because the operator asked for shell interpretation -- which is
		// why this verb keeps a Build.
		Trailing: &Trailing{
			Name: "command", List: true, AfterSeparator: true, Required: true, FieldArgs: "Argv", JoinWhen: "shell",
			Reason: "everything after `--` is the argv verbatim; re-scanning it for flags is how a command whose own first word is --shell gets eaten",
		},
		Flags: []Flag{
			{Name: "shell", Type: FlagBool, Default: false, Field: "Shell",
				Help: "hand it to the RUNNER's shell as one line (sh -c / cmd /c by its platform)"},
			{Name: "sshd-parent", Type: FlagBool, Default: false, Field: "SshdParent",
				Help: "run under the task's sshd parent process"},
		},
		// What it renames IS the shell, so it cannot rename an argv.
		Requires: []Requirement{{Flags: []string{"sshd-parent"}, Needs: "shell"}},
		Validate: func(b Bound) error {
			// `exec --shell kill 3` used to parse as "run `3` on a task named
			// kill". ls and kill are sub-verbs, and the run flags do not apply
			// to them, so naming one here is a mistake rather than a task id.
			if sub := b.Args[0]; sub == "ls" || sub == "kill" {
				return fmt.Errorf("exec: %q is a sub-verb; --shell / --sshd-parent do not apply to it", sub)
			}
			return nil
		},
		Examples: []string{"exec aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -- ls -la"},
	},
	{
		Path: []string{"exec", "ls"},
		Notes: []string{
			"list running execs; --task filters, --json emits JSON lines",
		},
		Action:   "ExecRunAction",
		Const:    map[string]string{"Sub": "ls"},
		Surfaces: CLI | TUI | WebUI,
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Field: "TaskFilter", Help: "only execs against this task id"},
			// --json was CLI-only before the migration; declaring it once gives
			// it to the surfaces that silently lacked it.
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON",
				Surfaces:      CLI | WebUI,
				SurfaceReason: "the TUI renders into a results pane, not a pipe, so there is nothing for JSON to be read by",
				Help:          "one JSON object per exec"},
		},
		Examples: []string{"exec ls", "exec ls --json"},
	},
	{
		Path: []string{"exec", "kill"},
		Notes: []string{
			"stop one or more running execs by id (from `exec ls`)",
		},
		// At least one id: `forward kill` with none is a mistyped line, not a
		// request to kill nothing.
		MinArgs:  1,
		Action:   "ExecRunAction",
		Const:    map[string]string{"Sub": "kill"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "exec-id", Type: ArgUint, Variadic: true, Field: "ExecIDs"}},
		Examples: []string{"exec kill 3"},
	},

	// --- forward ---
	//
	// Opening a forward is CLI-only: the TUI opens one from its port-forward
	// pane and a browser cannot bind a local listener at all. ls / kill / tap
	// are on all three.
	{
		// Opening a forward. CLI-only: the TUI opens one from its port-forward
		// pane and a browser cannot bind a local listener. The id used to be
		// read before the FlagSet was built, which meant the positional had to
		// come FIRST -- the inverse of what ParsePermuted guarantees
		// everywhere else. Declaring it removes that constraint.
		Path: []string{"forward"},
		Notes: []string{
			"-L: forward a local port through the runner to remote host:port (ssh -L)",
			"-R: runner listens, connections dial back to a client-side host:port (ssh -R)",
			"both repeatable; Ctrl-C to stop",
			"-W: raw stdio forward (ssh -W): no local listener, this process's stdin/stdout is the client endpoint",
			"with -W and --http-path: send one built HTTP request and stream the response (stdin is not spliced)",
			"-W is mutually exclusive with -L / -R; not repeatable; exits with its peer",
		},
		Surfaces: CLI,
		Action:   "ForwardOpenAction",
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		// -W owns the foreground and exits with its peer, while -L/-R are
		// long-lived listeners. ssh makes the same pair exclusive, for the
		// same reason: one invocation, one lifetime. Two pairs rather than one
		// group of three, because -L WITH -R is the ordinary case.
		Exclusive: []Rule{
			{Flags: []string{"W", "L"}, Reason: "-W owns the foreground and exits with its peer; -L is a long-lived listener"},
			{Flags: []string{"W", "R"}, Reason: "-W owns the foreground and exits with its peer; -R is a long-lived listener"},
		},
		AtLeastOne: []Rule{{Flags: []string{"L", "R", "W"}, Reason: "a forward with no direction opens nothing"}},
		Requires: []Requirement{{Flags: []string{"http-path"}, Needs: "W",
			Reason: "an HTTP request needs a target to send it to"}},
		Flags: []Flag{
			{Name: "L", Type: FlagString, Custom: argListValue, Field: "L",
				Help: "local forward [bind:]localport:remotehost:remoteport (repeatable)"},
			{Name: "R", Type: FlagString, Custom: argListValue, Field: "R",
				Help: "remote forward [bind:]runnerport:dialhost:dialport (repeatable)"},
			{Name: "W", Type: FlagString, Default: "", Field: "W",
				Help: "raw stdio forward host:port (mutually exclusive with -L / -R)"},
			{Name: "http-method", Type: FlagString, Default: "GET", Field: "HTTPMethod",
				Help: "with -W --http-path: HTTP method"},
			{Name: "http-path", Type: FlagString, Default: "", Field: "HTTPPath",
				Help: "with -W: send this HTTP request path instead of splicing stdin"},
			{Name: "http-body", Type: FlagString, Default: "", Field: "HTTPBody",
				Help: "with --http-path: request body (literal, @file, or - for stdin)"},
			{Name: "http-header", Type: FlagString, Custom: argListValue, Field: "HTTPHeaders",
				Help: `with --http-path: "Name: value" (repeatable)`},
		},
		Examples: []string{"forward aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -L 8080:localhost:80"},
	},
	{
		Path: []string{"forward", "ls"},
		Notes: []string{
			"list registered port forwards; --task filters, --json emits JSON lines",
		},
		Action:   "ForwardLsAction",
		Surfaces: CLI | TUI | WebUI,
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Field: "TaskFilter", Help: "only forwards for this task id"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON",
				Surfaces:      CLI | WebUI,
				SurfaceReason: "the TUI renders into a results pane, not a pipe, so there is nothing for JSON to be read by",
				Help:          "one JSON object per forward"},
		},
		Examples: []string{"forward ls", "forward ls --json"},
	},
	{
		Path: []string{"forward", "kill"},
		Notes: []string{
			"kill one or more registered forwards by id (from `forward ls`)",
		},
		// At least one id: `forward kill` with none is a mistyped line, not a
		// request to kill nothing.
		MinArgs:  1,
		Action:   "ForwardKillAction",
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "forward-id", Type: ArgUint, Variadic: true, Field: "ForwardIDs"}},
		Examples: []string{"forward kill 7"},
	},
	{
		Path: []string{"forward", "tap"},
		Notes: []string{
			"stream the bytes crossing one forward. A tap sees only what crosses AFTER it opens; nothing is recorded server-side",
			"--raw writes payload bytes with no headers, so it needs an explicit --dir: two directions on one stdout is not a stream any decoder can read",
		},
		Surfaces: CLI | TUI | WebUI,
		Action:   "ForwardTapAction",
		Args:     []Arg{{Name: "forward-id", Type: ArgUint, Field: "ForwardID"}},
		// The four render modes are one choice, not four bools.
		// They were CLI-only before -- the TUI took only --dir/--max-bytes.
		Modes: &Modes{Field: "Mode", Names: []string{"hex", "text", "raw", "json"}, Default: "hex"},
		// The four render modes shape a byte STREAM on stdout. The WebUI's tap
		// is a scrolling panel with its own rendering and no stdout to shape,
		// so all four parsed there and were discarded -- declared away rather
		// than silently ignored. --dir and --max-bytes are honoured on all
		// three: they decide what is captured, not how it is printed.
		Flags: []Flag{
			{Name: "dir", Type: FlagString, Default: "both", Field: "Dir",
				OneOf: []string{"to-target", "from-target", "both"},
				Help:  "to-target, from-target or both"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Field: "MaxRecordBytes", FieldType: "uint32",
				Help: "cut each record's payload to this many bytes (0 = whole payload)"},
			{Name: "hex", Type: FlagBool, Default: false, FieldReason: "the mode group carries it",
				Surfaces: CLI | TUI, SurfaceReason: "a browser tap is a panel, not a stream on stdout",
				Help: "hexdump body (default)"},
			{Name: "text", Type: FlagBool, Default: false, FieldReason: "the mode group carries it",
				Surfaces: CLI | TUI, SurfaceReason: "a browser tap is a panel, not a stream on stdout",
				Help: "printable body, no offset column"},
			{Name: "raw", Type: FlagBool, Default: false, FieldReason: "the mode group carries it",
				Surfaces: CLI | TUI, SurfaceReason: "a browser tap is a panel, not a stream on stdout",
				Help: "payload bytes only; requires an explicit --dir"},
			{Name: "json", Type: FlagBool, Default: false, FieldReason: "the mode group carries it",
				Surfaces: CLI | TUI, SurfaceReason: "a browser tap is a panel, not a stream on stdout",
				Help: "one JSON object per record"},
		},
		// --raw writes payloads with no headers, so two directions
		// concatenated onto one stdout interleave two conversations into a
		// byte soup no decoder can read. No attribute says "this VALUE of one
		// flag forbids that value of another", so it stays code.
		Validate: func(b Bound) error {
			// The field is uint32 on the wire, so a larger number silently
			// became a small cut: 4294967297 read as 1 byte.
			if mb := uintOf(b.Flags["max-bytes"]); uint64(mb) > math.MaxUint32 {
				return fmt.Errorf("forward tap: --max-bytes %d is out of range", mb)
			}
			if b.Bool("raw") && b.Str("dir") == "both" {
				return fmt.Errorf("forward tap: --raw needs an explicit --dir (to-target or from-target); " +
					"both directions on one stdout is not a stream any decoder can read")
			}
			return nil
		},
		Examples: []string{"forward tap 7", "forward tap 7 --dir to-target --text"},
	},

	// --- server ---
	{
		Path: []string{"server", "dial-runner"},
		Notes: []string{
			"ask the server to reverse-dial RUNNER_CID (Phase A/B)",
			"--via relays through an already-connected runner (Phase B)",
			"(the runner must be running in --listen / --udp-listen mode)",
			"prints the DialRunnerStatus and exits non-zero on non-Ok",
		},
		Action:   "ServerDialRunnerAction",
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "runner-cid", Type: ArgString, Field: "RunnerCID"}},
		Flags: []Flag{
			{Name: "via", Type: FlagString, Default: "", Field: "Via",
				Help: "relay through this registered runner CID (copy from `harness-cli ls`)"},
		},
		Examples: []string{"server dial-runner ws:127.0.0.1:9000-abcd"},
	},

	// --- ssh-gateway ---
	//
	// Two shapes, not one: the CLI runs the gateway in the foreground with
	// flags, while the TUI starts and stops a background one. Declared as
	// separate paths because they are separate operations wearing one name.
	{
		Path: []string{"ssh-gateway"},
		Notes: []string{
			"serve ssh: `ssh -p 2222 <32-hex-task-id>@127.0.0.1` attaches to that session,",
			"so ssh config aliases, tmux and mosh reach a task with no harness binary there",
			"the user name picks the mode: bare = cowrite (evicts nobody), .control takes",
			"the seat and owns the PTY size, .view watches",
			"Ctrl+] detaches. ssh's own ~. DISCONNECTS instead: the session survives either",
			"way, but a disconnect leaves your terminal's modes unreset (`reset` fixes it)",
			"no ssh auth on a loopback bind; --authorized-keys is REQUIRED off loopback",
			"ssh -L / -W tunnel through it: the RUNNER dials the target, and each",
			"forwarded connection is an ordinary `forward ls` row while it lasts",
			"no scp/sftp and no ssh -R: use `file push`/`file pull` and `forward -R`",
			"foreground; Ctrl-C stops it and every session it serves",
		},
		Action:   "SSHGatewayAction",
		Surfaces: CLI,
		Flags: []Flag{
			{Name: "listen", Type: FlagString, Default: "127.0.0.1:2222", Field: "Listen",
				Help: "ssh listen host:port (no ssh auth on a loopback bind; --authorized-keys is required off loopback)"},
			{Name: "host-key", Type: FlagString, Default: "", Field: "HostKeyPath",
				Help: "ssh host key path (default: alongside the workspace config; generated on first run)"},
			{Name: "authorized-keys", Type: FlagString, Default: "", Field: "AuthorizedKeys",
				Help: "OpenSSH authorized_keys file; optional on a loopback bind, required otherwise"},
		},
		Examples: []string{"ssh-gateway", "ssh-gateway --listen 127.0.0.1:2223"},
	},
	// The TUI's form is a different SHAPE, not the same verb with a narrower
	// flag set: the CLI runs the gateway in the foreground with flags, while
	// the TUI starts and stops a background one it hosts. Two paths, as the
	// design says -- and declared here rather than hand-parsed, which is what
	// they were: the last verb-shaped token walk in tui/cmdline.go.
	{
		Path:     []string{"ssh-gateway", "start"},
		Action:   "SSHGatewayAction",
		Const:    map[string]string{"Sub": "start"},
		Surfaces: TUI,
		// The default is the CLI flag's, declared once: it lived here as a
		// fallback inside a hand-written parser and there as Flag.Default,
		// which is two places for one address.
		Args: []Arg{{Name: "bind-addr", Type: ArgString, Variadic: true, MaxCount: 1, Field: "Listen",
			Default:  "127.0.0.1:2222",
			Surfaces: TUI, SurfaceReason: "the CLI names the same address with --listen, in the foreground form"}},
		Examples: []string{"ssh-gateway start", "ssh-gateway start 127.0.0.1:2223"},
	},
	{
		// Bare `ssh-gateway` reports; declared as its own path so the family
		// answers with ONE type. It had a local type for the status form and
		// the declaration's for the other two, which is the shadowing footgun
		// exactly: two types with the same name, both satisfying Action, and a
		// `case SSHGatewayAction:` written by habit never matches.
		Path:     []string{"ssh-gateway", "status"},
		Action:   "SSHGatewayAction",
		Const:    map[string]string{"Sub": "status"},
		Surfaces: TUI,
		Examples: []string{"ssh-gateway status"},
	},
	{
		Path:     []string{"ssh-gateway", "stop"},
		Action:   "SSHGatewayAction",
		Const:    map[string]string{"Sub": "stop"},
		Surfaces: TUI,
		Examples: []string{"ssh-gateway stop"},
	},

	// --- workspace ---
	{
		Path: []string{"workspace", "save"},
		Notes: []string{
			"records every task the registry reports a forward for; --task narrows it to one,",
			"and is also how a task's forwards get CLEARED after you stop them.",
			"It MERGES: task blocks it did not observe are kept, and an existing block's",
			"resume / runner are never overwritten -- those are yours to hand-edit.",
			"(in-process forwards -- a raw TUI pane, a WebUI preview pin -- have no local",
			"address to write down and are skipped, with a count)",
		},
		Action: "WorkspaceAction",
		Const:  map[string]string{"Sub": "save"},
		// A half-typed id would filter to nothing and record an empty
		// workspace, which reads as "this task has no forwards".
		Validate: func(b Bound) error {
			id := b.Str("task")
			if id == "" {
				return nil
			}
			if _, err := hex.DecodeString(id); err != nil || len(id) != 32 {
				return fmt.Errorf("workspace save: --task must be a 32-hex task id, got %q", id)
			}
			return nil
		},
		Surfaces: CLI | TUI,
		Args:     []Arg{{Name: "name", Type: ArgString, Field: "Name"}},
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Field: "TaskID", Surfaces: CLI,
				SurfaceReason: "the TUI picks the tasks in a picker instead of naming one on the line",
				Help:          "record only this task (32 hex); omitted = every task the registry reports a forward for"},
			{Name: "resume", Type: FlagString, Default: "continue", Field: "Resume", Surfaces: CLI,
				SurfaceReason: "written through the TUI's picker rather than a flag",
				Help:          "no | continue | fresh — for a task block being written for the FIRST time"},
			{Name: "runner", Type: FlagString, Default: "assigned", Field: "Runner", Surfaces: CLI,
				SurfaceReason: "written through the TUI's picker rather than a flag",
				Help:          "assigned | any — for a task block being written for the FIRST time"},
			{Name: "repo", Type: FlagString, Default: "", Field: "Repo", Surfaces: CLI,
				SurfaceReason: "the TUI already knows its repo from the session",
				Help:          "repo identifier to record in the workspace"},
			{Name: "all", Type: FlagBool, Default: false, Field: "All", Surfaces: TUI,
				SurfaceReason: "skips the TUI's task picker; the CLI has no picker to skip",
				Help:          "write every live session without opening the picker"},
		},
		Examples: []string{"workspace save dev"},
	},
	{
		Path: []string{"workspace", "rm"},
		Notes: []string{
			"delete one workspace from .harness/config (other workspaces and comments kept)",
		},
		Action:   "WorkspaceAction",
		Const:    map[string]string{"Sub": "rm"},
		Surfaces: CLI | TUI,
		// A name is required, and there is no "the current one" shorthand:
		// deleting is the one verb here that cannot be undone by re-running it.
		Args:     []Arg{{Name: "name", Type: ArgString, Field: "Name"}},
		Examples: []string{"workspace rm dev"},
	},
	{
		Path: []string{"workspace", "ls"},
		Notes: []string{
			"list the workspaces in .harness/config",
		},
		Action:   "WorkspaceAction",
		Const:    map[string]string{"Sub": "ls"},
		Surfaces: CLI | TUI,
		Examples: []string{"workspace ls"},
	},
	{
		Path: []string{"workspace", "show"},
		Notes: []string{
			"print one workspace, or all of them when no name is given",
		},
		Action:   "WorkspaceAction",
		Const:    map[string]string{"Sub": "show"},
		Surfaces: CLI | TUI,
		Args:     []Arg{{Name: "name", Type: ArgString, Variadic: true, MaxCount: 1, Field: "Name"}},
		Examples: []string{"workspace show", "workspace show dev"},
	},
	{
		Path:   []string{"workspace", "apply"},
		Action: "WorkspaceAction",
		Const:  map[string]string{"Sub": "apply"},
		// TUI-only: applying establishes forwards and resumes tasks, and a
		// forward dies with the process that holds it -- so there is nothing
		// for a one-shot CLI invocation to apply. usage() says as much.
		Surfaces: TUI,
		Args:     []Arg{{Name: "name", Type: ArgString, Variadic: true, MaxCount: 1, Field: "Name"}},
		Examples: []string{"workspace apply", "workspace apply dev"},
	},
	{
		Path:     []string{"workspace", "detach"},
		Action:   "WorkspaceAction",
		Const:    map[string]string{"Sub": "detach"},
		Surfaces: TUI,
		// Takes no name on purpose: there is only ever one installed
		// workspace, and accepting a name would invite `detach other` to read
		// as "detach that one instead of mine".
		Flags: []Flag{
			{Name: "stop", Type: FlagBool, Default: false, Field: "Stop",
				Help: "also stop what the workspace started"},
		},
		Examples: []string{"workspace detach", "workspace detach --stop"},
	},

	// --- board ---
	//
	// CLI-only: the TUI and WebUI reach the agentboard through dedicated
	// panes, not a command line. This is the family whose `purge <topic>
	// --seq N` destroyed two live messages, which is why --seq is marked as
	// widening when unset.
	{
		Path: []string{"board", "topics"},
		Notes: []string{
			"list every topic on the board with metadata (cap: board_observe)",
		},
		Surfaces: CLI,
		Examples: []string{"board topics"},
		Action:   "BoardAction",
		Const:    map[string]string{"Sub": "topics"},
	},
	{
		Path: []string{"board", "read"},
		Notes: []string{
			"print retained messages for <topic> (text: header + pretty payload;",
			"--json: JSON Lines, the same record shape as `agent inbox --json`; not found = exit 0)",
		},
		Surfaces: CLI,
		Action:   "BoardAction",
		Const:    map[string]string{"Sub": "read"},
		Args:     []Arg{{Name: "topic", Type: ArgTopic, Field: "Topic"}},
		Flags: []Flag{
			{Name: "in-reply-to", Type: FlagUint64, Default: uint64(0), Field: "InReplyTo",
				Help: "only messages replying to this seq"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "JSON Lines instead of text"},
		},
		Examples: []string{"board read chat.abcd1234", "board read chat.abcd1234 --json"},
	},
	{
		Path: []string{"board", "subscribers"},
		Notes: []string{
			"list each task's subscriptions; with <topic>, only the tasks a publish there reaches (cap: board_observe)",
		},
		Surfaces: CLI,
		Action:   "BoardAction",
		Const:    map[string]string{"Sub": "subscribers"},
		// At most one, expressed as arity rather than as a check in Build:
		// MaxArgs is what the declaration already knows.
		Args:     []Arg{{Name: "topic", Type: ArgTopic, Variadic: true, MaxCount: 1, Field: "Topic"}},
		Examples: []string{"board subscribers", "board subscribers chat.abcd1234"},
	},
	{
		Path: []string{"board", "retract"},
		Notes: []string{
			"withdraw one message: gone from every agent path, still readable here until the topic ages out.",
			"--seq is required -- there is no whole-topic retract (cap: purge)",
		},
		Surfaces: CLI,
		Action:   "BoardAction",
		Const:    map[string]string{"Sub": "retract"},
		Args:     []Arg{{Name: "topic", Type: ArgTopic, Field: "Topic"}},
		// Required is presence, and presence is not enough here: --seq 0 is
		// purge's "the whole topic", and withdrawing a topic-full of other
		// agents' messages on a mistyped flag is exactly the accident this
		// verb must not be able to have. There is no whole-topic retract.
		Validate: func(b Bound) error {
			if uint64Of(b.Flags["seq"]) == 0 {
				return fmt.Errorf("board retract: --seq must be non-zero (there is no whole-topic retract; `board read %s` lists the seqs)", b.Args[0])
			}
			return nil
		},
		Flags: []Flag{
			{Name: "seq", Type: FlagUint64, Default: uint64(0), Required: true, Field: "Seq",
				Help: "the message to withdraw; required — there is no whole-topic retract"},
		},
		Examples: []string{"board retract chat.abcd1234 --seq 42"},
	},
	{
		Path: []string{"board", "purge"},
		Notes: []string{
			"drop the whole topic ring (seq=0) or one message by seq.",
			"Unlike retract this destroys the bytes, operator view included (cap: purge)",
		},
		Surfaces: CLI,
		Action:   "BoardAction",
		Const:    map[string]string{"Sub": "purge"},
		Args:     []Arg{{Name: "topic", Type: ArgTopic, Field: "Topic"}},
		Flags: []Flag{
			// THE flag this whole design is named after. `board purge <topic>
			// --seq N` -- the exact line the help text printed -- left --seq at
			// its zero value under stdlib parsing, which is the WHOLE-TOPIC
			// form, and destroyed two messages on a live board.
			{Name: "seq", Type: FlagUint64, Default: uint64(0), WidensIfUnset: true, Field: "Seq",
				Help: "drop one message by seq; omitted drops the whole topic ring"},
		},
		Examples: []string{"board purge chat.abcd1234", "board purge chat.abcd1234 --seq 42"},
	},
	// --- spawning: submit / interactive / session new ---
	//
	// One shape, three verbs. The inventory found the TUI's submit had no
	// --runner/--host/--ip at all and its session new no --repo/--rows/--cols,
	// and that none of the three accepted --agent-arg -- only the CLI's
	// deprecated --claude-arg spelling, because the TUI's copy was written
	// before the rename and its comment still says so. Declaring the family
	// once gives every surface the same set.
	{
		Path: []string{"submit"},
		Notes: []string{
			"enqueue a new task (--repo: HARNESS_REPO_PATH)",
			"--agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias",
			"--resume reuses an existing terminal task id + worktree branch (so `--agent-arg --resume <uuid>` forwards the agent's stored-session flag)",
		},
		Surfaces: CLI | TUI | WebUI,
		// The prompt is a positional on the TUI and the WebUI and --task on the
		// CLI. Both work everywhere now: the flag wins when given, the trailing
		// words are the prompt otherwise.
		Action: "SpawnAction",
		Const:  map[string]string{"Kind": "submit"},
		Trailing: &Trailing{
			Name: "prompt", Field: "Task", IfFieldEmpty: true,
			Reason: "the prompt is free-form text, so a word beginning with '-' cannot be told from a flag",
		},
		Flags:     spawnFlags(spawnSubmit),
		Exclusive: spawnExclusive(spawnSubmit),
		Requires:  spawnRequires(spawnSubmit),
		// A rule spanning a flag and the trailing text, which no attribute
		// reaches: Trailing.Required would refuse the --task form.
		Validate: func(b Bound) error {
			if err := spawnValidate(b); err != nil {
				return fmt.Errorf("submit: %w", err)
			}
			if b.Str("task") == "" && b.Trail == "" {
				return fmt.Errorf("submit: a prompt is required, as --task or as the trailing words")
			}
			return nil
		},
		Examples: []string{`submit --repo /r --task "do the thing"`, `submit --repo /r do the thing`},
	},
	{
		Path: []string{"interactive"},
		Notes: []string{
			"attach an interactive PTY agent; the session is detachable (--repo: HARNESS_REPO_PATH)",
			"--agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias",
			"--resume reuses an existing terminal interactive task id + worktree branch",
		},
		Surfaces:  CLI | TUI,
		Action:    "SpawnAction",
		Const:     map[string]string{"Kind": "interactive"},
		Flags:     spawnFlags(spawnInteractive),
		Exclusive: spawnExclusive(spawnInteractive),
		Requires:  spawnRequires(spawnInteractive),
		Validate: func(b Bound) error {
			if err := spawnValidate(b); err != nil {
				return fmt.Errorf("interactive: %w", err)
			}
			return nil
		},
		Examples: []string{"interactive --repo /r"},
	},
	{
		Path: []string{"session", "new"},
		Notes: []string{
			"open a detachable interactive PTY session (--repo: HARNESS_REPO_PATH)",
			"-d / --detach: start the session and exit immediately (don't attach the terminal)",
		},
		Surfaces:  CLI | TUI,
		Action:    "SpawnAction",
		Const:     map[string]string{"Kind": "session-new"},
		Flags:     spawnFlags(spawnSessionNew),
		Exclusive: spawnExclusive(spawnSessionNew),
		Requires:  spawnRequires(spawnSessionNew),
		Validate: func(b Bound) error {
			if err := spawnValidate(b); err != nil {
				return fmt.Errorf("session new: %w", err)
			}
			return nil
		},
		Examples: []string{"session new --repo /r", "session new --repo /r -d"},
	},
	// --- the six Trailing verbs ---
	//
	// Their trailing words are literal text, so a '-'-leading word cannot be
	// told from a flag and the parse CANNOT permute. Six, not the four on
	// cli/flagorder_test.go's allowlist: `agent send` and `agent dispatch`
	// take a joined-positional payload too, and were invisible to that guard
	// because they read their positionals inside resolvePayload rather than
	// off the FlagSet.
	{
		Path: []string{"session", "send"},
		Notes: []string{
			"inject input into a session (co-writer attach, no takeover); pair with snapshot to drive it statelessly",
			"--enter appends a CR (i.e. actually submits); -e interprets \\n \\r \\t \\e \\xHH",
			"flags must precede <task-id>; everything after it is joined with spaces and sent literally",
		},
		Surfaces: CLI,
		Action:   "SendAction",
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Trailing: &Trailing{Name: "text", Field: "Text", Required: true,
			Reason: "the literal text to type into the PTY"},
		// The snapshot knobs only mean something with --snapshot. Naming one
		// without it is refused rather than ignored: a caller who asked for 80
		// columns and silently got the default is debugging the wrong thing.
		Requires: []Requirement{{
			Flags: []string{"rows", "cols", "settle-ms", "style", "color", "json",
				"ansi", "without-synth", "detect", "detect-agent"},
			Needs: "snapshot",
		}},
		Flags: []Flag{
			// THE pair this design's alias rule exists for. --enter appends a
			// carriage return; -e interprets backslash escapes. They are two
			// flags, not a long form and its short form, and merging them
			// would turn `session send -e '...'` into a spurious Enter typed
			// into a live PTY -- while compiling and reviewing cleanly.
			{Name: "enter", Type: FlagBool, Default: false, Field: "Enter",
				Help: "append a carriage return (Enter) after the text"},
			{Name: "e", Type: FlagBool, Default: false, Field: "Interp",
				Help: `interpret backslash escapes (\n \r \t \e \xHH \\)`},
			{Name: "quiet", Type: FlagBool, Default: false, Field: "Quiet",
				Help: "suppress the one-line summary of what was sent (stderr)"},
			{Name: "flush-ms", Type: FlagUint, Default: uint(400), Field: "FlushMs",
				Help: "ms to let the input drain to the runner before detaching"},
			{Name: "resize", Type: FlagString, Default: "", Field: "Resize",
				Help: "before sending, set the PTY size to ROWSxCOLS (e.g. 40x150)"},
			{Name: "snapshot", Type: FlagBool, Default: false, Field: "Snapshot",
				Help: "after sending, render the session's screen to stdout"},
			{Name: "rows", Type: FlagUint, Default: uint(40), Field: "Rows", Help: "with --snapshot: fallback rows"},
			{Name: "cols", Type: FlagUint, Default: uint(120), Field: "Cols", Help: "with --snapshot: fallback cols"},
			{Name: "settle-ms", Type: FlagUint, Default: uint(1500), Field: "SettleMs",
				Help: "with --snapshot: ms to collect output before rendering"},
			{Name: "style", Type: FlagBool, Default: false, Field: "Style", Help: "with --snapshot: also print attribute spans"},
			{Name: "color", Type: FlagBool, Default: false, Field: "Color", Help: "with --snapshot: also print colour spans"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "with --snapshot: emit the screen as one JSON object"},
			{Name: "ansi", Type: FlagBool, Default: false, Field: "ANSI", Help: "with --snapshot: re-emit the screen WITH its colours"},
			{Name: "without-synth", Type: FlagBool, Default: false, Field: "WithoutSynth",
				Help: "with --snapshot: render only what the PTY produced, dropping the server's replay additions"},
			{Name: "detect", Type: FlagBool, Default: false, Field: "Detect",
				Help: "with --snapshot: judge the resulting state (working / blocked / idle / unknown)"},
			{Name: "detect-agent", Type: FlagString, Default: "claude", Field: "DetectAgent",
				Help: "with --detect: which agent's rule set to judge by"},
		},
		Examples: []string{
			"session send aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa hello world",
			"session send --enter aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa yes",
			`session send -e aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa line\none`,
			`session send -e --enter aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa done\t`,
		},
	},
	{
		Path: []string{"session", "exec"},
		Notes: []string{
			"run one shell command line in the session's foreground shell and block until it finishes",
			"exits with the command's own code (124 timeout, 125 error, 126 foreground shell exited); needs a POSIX shell",
			"NOT `exec`, which runs its own process in the worktree with separate stdout/stderr",
			"flags must precede <task-id>; everything after it is joined with spaces as the command line",
		},
		Surfaces: CLI,
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Action:   "SessionExecAction",
		Trailing: &Trailing{Name: "command", Field: "Cmd", Required: true,
			Reason: "the command line to run in the session's foreground shell"},
		Flags: []Flag{
			{Name: "timeout", Type: FlagDuration, Default: 30 * time.Second, Field: "Timeout",
				Help: "max wait for the command to finish before giving up (exit 124)"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON",
				Help: `emit {"exit":N,"output":"…","timed_out":bool,"duration_ms":N} as one JSON object`},
			{Name: "exit-only", Type: FlagBool, Default: false, Field: "ExitOnly",
				Help: "print no output; only propagate the exit code"},
			{Name: "raw", Type: FlagBool, Default: false, Field: "Raw",
				Help: "return the verbatim output bytes (escape sequences intact)"},
		},
		Examples: []string{"session exec aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ls -la"},
	},
	{
		Path: []string{"session", "stream", "turn"},
		Notes: []string{
			"send one user turn to an event-stream session",
		},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		// SessionAction like the rest of the family, not a type of its own:
		// the TUI dispatches the namespace on Sub, and one verb answering with
		// a different type made that verb the one that stayed hand-parsed.
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "stream-turn"},
		Trailing: &Trailing{Name: "text", Field: "Text", Required: true, Reason: "the user turn's text"},
		Flags: []Flag{
			{Name: "flush-ms", Type: FlagUint, Default: uint(400), Field: "FlushMs",
				Help: "ms to let the line drain to the runner before detaching"},
		},
		Examples: []string{"session stream turn aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa please continue"},
	},
	{
		Path: []string{"notify"},
		Notes: []string{
			"send a notification (one short line; detail goes in the task log)",
		},
		Action:   "NotifyAction",
		Surfaces: CLI | TUI,
		Trailing: &Trailing{Name: "text", Field: "Text", Required: true,
			Reason: "the notification body is free-form"},
		Flags: []Flag{
			{Name: "title", Type: FlagString, Default: "", Field: "Title", Help: "short heading for the notification"},
			{Name: "level", Type: FlagString, Default: "info", Field: "Level",
				OneOf: []string{"info", "warn", "error"}, Help: "severity: info|warn|error"},
		},
		Examples: []string{"notify --level warn --title build the tree is red"},
	},
	{
		Path: []string{"agent", "send"},
		Notes: []string{
			"publish a message. The body is the trailing words, or --data STRING, or --data - to read it from stdin.",
			"A bare \"-\" is a VALUE OF --data, never a positional: `send --topic T -` publishes the one-byte body \"-\".",
			"The ok line reports bytes and source, so a body that went out wrong says so at once.",
			"--in-reply-to SEQ replies to that message; --topic is then optional (the server routes it where that message asked).",
			"--reply-to R routes replies to THIS message to R instead of your own chat.<short-id>; the peer answers with --in-reply-to alone.",
		},
		Surfaces: CLI,
		Action:   "AgentSendAction",
		Const:    map[string]string{"Kind": "send"},
		Trailing: &Trailing{Name: "text", Field: "Positional",
			Reason: "the message body is free-form; --data or stdin are the alternatives"},
		Flags:    agentSendFlags(false),
		Examples: []string{"agent send --topic chat.abcd1234 hello there"},
	},
	{
		Path: []string{"agent", "dispatch"},
		Notes: []string{
			"send, then block for the reply to THAT message. --reply-to R declares R as the destination AND waits there;",
			"default is your own chat.<short-id>. --timeout bounds the WHOLE call, publish ack included",
			"(scripting; NOT from an agent turn)",
		},
		Surfaces: CLI,
		Action:   "AgentSendAction",
		Const:    map[string]string{"Kind": "dispatch"},
		Trailing: &Trailing{Name: "text", Field: "Positional",
			Reason: "the message body is free-form; --data or stdin are the alternatives"},
		Flags:    agentSendFlags(true),
		Examples: []string{"agent dispatch --topic chat.abcd1234 do the thing"},
	},
	// --- listings and catalogs ---
	{
		// The grid grammar was ALREADY shared before this migration --
		// cli.ParseGridArgs, which the workspace config validates against so a
		// saved selection cannot name a spelling the command rejects. This
		// entry routes the command inputs through the same table as the rest;
		// the parse itself still delegates to that function.
		Path: []string{"grid"}, Surfaces: TUI | WebUI,
		Action:  "GridAction",
		Args:    []Arg{{Name: "task-id", Type: ArgTaskID, Variadic: true, Field: "IDs"}},
		Derived: []Derived{{Field: "Mode", Type: "GridScopeMode", From: "gridMode"}},
		Flags: []Flag{
			{Name: "under", Type: FlagString, Default: "", Field: "Anchor",
				Help: "the anchor whose working set to show: itself, its descendants, and the tasks its own scope names"},
			{Name: "descendants", Type: FlagBool, Default: false,
				FieldReason: "it selects a Mode rather than travelling as its own flag",
				Help:        "with --under: the descendants only, leaving the anchor out"},
		},
		Examples: []string{"grid", "grid --under aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"cancel"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"cancel a queued/running task",
		},
		Action:   "CancelAction",
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Examples: []string{"cancel aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"ls"}, Surfaces: CLI | WebUI,
		Notes: []string{
			"list runners and recent tasks; --json emits one {runners,tasks} object",
		},
		Action: "ListAction",
		// --json already carries created_by on every row, so a consumer
		// builds the tree itself; nesting the JSON to match would give the
		// same data two shapes.
		Exclusive: []Rule{{Flags: []string{"tree", "json"}}},
		Flags: []Flag{
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON",
				Help: `emit a single JSON object {"runners":[...],"tasks":[...]} instead of the table`},
			{Name: "tree", Type: FlagBool, Default: false, Field: "Tree",
				Help: "order tasks by their creator link and draw the hierarchy"},
			{
				Name: "filtered", Type: FlagBool, Default: false, Surfaces: WebUI, Field: "Filtered",
				SurfaceReason: "only the WebUI has a task-list filter pane; the CLI has no filter to honour and the TUI's only filter is on the logs panel",
				Help:          "list only the rows the task-list filter currently admits",
			},
		},
		Examples: []string{"ls", "ls --json", "ls --tree"},
	},
	{
		Path: []string{"conns"}, Surfaces: CLI,
		Notes: []string{
			"snapshot live connections (requires info_global cap); -f streams live events; --json emits JSON lines",
		},
		Action: "ConnsAction",
		Flags: []Flag{
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "output JSON lines instead of a table"},
			{Name: "follow", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Follow",
				Help: "stream live connection events (conns.status)"},
		},
		Examples: []string{"conns", "conns -f --json"},
	},
	{
		// All three surfaces. The catalog is the only place the granular
		// capabilities carry a description of what they actually gate, and it
		// used to be reachable from the CLI alone -- so a TUI or WebUI
		// operator picking chips had the names and not the sentences.
		Path: []string{"caps"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"list the grantable --caps capability names and --scope forms",
		},
		Action:   "CatalogAction",
		Const:    map[string]string{"Sub": "caps"},
		Flags:    []Flag{{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "output the capability catalog as JSON"}},
		Examples: []string{"caps", "caps --json"},
	},
	{
		// `skill ls` is this verb's own spelling of --list. It was a TOKEN
		// REWRITE in main (`if args[0] == "ls" { args[0] = "--list" }`),
		// which is the one shape a declaration cannot see: moving the CLI
		// onto the generated parser dropped it and `skill ls` started
		// looking for a skill NAMED "ls".
		//
		// Its own Const rather than a shared one, because Const values are
		// strings and --list is a bool: the two spellings reach the same
		// body through two methods, not one.
		Path: []string{"skill", "ls"}, Surfaces: CLI,
		Notes: []string{
			"name every embedded skill; the same as `skill --list`",
		},
		Action:   "CatalogAction",
		Const:    map[string]string{"Sub": "skill-ls"},
		Examples: []string{"skill ls"},
	},
	{
		Path: []string{"whoami"}, Surfaces: CLI,
		Notes: []string{
			"show THIS connection's own principal + server-enforced caps and scope (no cap required)",
		},
		Action:   "CatalogAction",
		Const:    map[string]string{"Sub": "whoami"},
		Flags:    []Flag{{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "output the identity as a JSON object"}},
		Examples: []string{"whoami"},
	},
	// Three verbs the migration dispatched and never declared -- found by the
	// usage guard, not by review: usage() printed them and PathsForSurface did
	// not know them, so every completeness check passed over them.
	{
		Path: []string{"skill"}, Surfaces: CLI,
		Notes: []string{
			"print the embedded agent skill (default: harness-cli)",
		},
		Action: "CatalogAction",
		Const:  map[string]string{"Sub": "skill"},
		Args: []Arg{{Name: "name", Type: ArgString, Variadic: true, MaxCount: 1, Field: "Name",
			Optional: true}},
		Flags: []Flag{{Name: "list", Aliases: []string{"l"}, Type: FlagBool, Default: false, Field: "List",
			Help: "name every embedded skill with its description instead of printing one"}},
		Examples: []string{"skill", "skill --list", "skill harness-cli"},
	},
	{
		Path: []string{"watch"}, Surfaces: CLI,
		Notes: []string{
			"stream task and runner status events",
		},
		Action:   "CatalogAction",
		Const:    map[string]string{"Sub": "watch"},
		Examples: []string{"watch"},
	},
	{
		Path: []string{"notify-watch"}, Surfaces: CLI,
		Notes: []string{
			"stream notifications (backlog + live); one human-readable line each",
		},
		Action:   "CatalogAction",
		Const:    map[string]string{"Sub": "notify-watch"},
		Examples: []string{"notify-watch"},
	},
	{
		Path: []string{"version"}, Surfaces: CLI,
		Notes: []string{
			"the commit this binary \u2014 and the skills embedded in it \u2014 was built from",
		},
		Action:   "CatalogAction",
		Const:    map[string]string{"Sub": "version"},
		Flags:    []Flag{{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "output the build stamp as a JSON object"}},
		Examples: []string{"version"},
	},
	{
		Path: []string{"logs"}, Surfaces: CLI,
		Notes: []string{
			"dump task log history; -f also streams live chunks until task terminal",
		},
		Action: "LogsAction",
		Args:   []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags: []Flag{
			{Name: "follow", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Follow",
				Help: "after dumping history, keep streaming live chunks (no-op when the task is terminal)"},
		},
		Examples: []string{"logs aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "logs -f aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"prune-local"}, Surfaces: CLI,
		Notes: []string{
			"remove worktrees in <repo>/.harness-worktrees/ (--repo: HARNESS_REPO_PATH)",
			"with no ids: time-based, removes entries older than --before",
			"with ids: removes only those (refuses active tasks unless --force)",
		},
		Action: "PruneLocalAction",
		// Same shape as `prune`, and it removes WORKTREES -- the half a
		// server-side prune leaves behind, and the only remaining copy of an
		// agent's work once the task entry is gone.
		AtLeastOne: []Rule{{Flags: []string{"task-id", "before"},
			Reason: "a bare prune-local removes every worktree older than the default; say which"}},
		Args: []Arg{{Name: "task-id", Type: ArgTaskID, Variadic: true, Field: "TaskIDs", WidensIfUnset: true}},
		Flags: []Flag{
			{Name: "repo", Type: FlagString, Default: ".", Field: "Repo", Help: "repo to prune",
				Resolve: []Tier{{Env: "HARNESS_REPO_PATH"}, {Workspace: "repo"}}},
			{Name: "before", Type: FlagDuration, Default: 7 * 24 * time.Hour, Field: "Before",
				Help: "remove worktrees older than this (ignored when TASK_IDs are passed)"},
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false, Field: "Force",
				Help: "with TASK_IDs: remove even when the server still considers the task active"},
		},
		Examples: []string{"prune-local --before 24h"},
	},

	{
		// The undo half of prune, and the reason prune's bare form had to go:
		// a sweep with no way back is a one-line accident. Rebuilt from the
		// server's own WAL -- every field comes from a record it wrote --
		// so this puts back what was forgotten and cannot invent a task.
		//
		// Ids are required and there is no --before: the WAL holds every task
		// the server has ever seen, and a sweep back would resurrect years of
		// them. The asymmetry with prune is deliberate.
		Path: []string{"restore"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"with no ids (or --list): list what a prune forgot and could still be put back \u2014 ids, when they were pruned, and the repo/prompt that identify them. The ids live only in the server's WAL, so this is the only way to learn them",
			"with ids: put those back, rebuilt from the WAL. Requires the `prune` capability and the same scope: what you could forget, you can un-forget",
			"the RECORD returns; the task log does not (prune removed the file) and the worktree was never touched. An id with no task_created cannot be rebuilt",
		},
		Action: "RestoreAction",
		Args:   []Arg{{Name: "task-id", Type: ArgTaskID, Variadic: true, Field: "TaskIDs"}},
		// No AtLeastOne here, unlike prune. The bare form LISTS -- it changes
		// nothing, and it is the only way to learn the ids, which live in a
		// file on the server host that no other surface reads. A restore verb
		// whose bare form did nothing would be usable only by someone who
		// wrote the id down before the accident.
		Flags: []Flag{{Name: "list", Aliases: []string{"l"}, Type: FlagBool, Default: false, Field: "List",
			Help: "list what a prune forgot and could still be put back (the default with no ids)"}},
		Examples: []string{
			"restore",
			"restore --list",
			"restore aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"restore aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	},

	// --- the screen-state verbs ---
	//
	// TUI-only, and declared for the reason the design gives (Derivation
	// §TUI): "only this surface has it" is what Surfaces says, not a reason to
	// sit outside the table. What is surface-local about them is the HANDLER
	// -- D3 -- and that is true of every verb here.
	//
	// They were a hand-written switch in tui/cmdline.go plus a hand-written
	// allowlist in cmdline_help_test.go, which is two more copies of a list
	// the table can hold.
	{
		Path: []string{"clear"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "clear"},
		Examples: []string{"clear"},
	},
	{
		Path: []string{"quit"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "quit"},
		Examples: []string{"quit"},
	},
	{
		// `exit` is its own path rather than an alias of quit: D16 refused a
		// path-alias mechanism because it preserves two spellings for one
		// verb, and two declared paths sharing an Action say the same thing
		// without the mechanism.
		Path: []string{"exit"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "quit"},
		Examples: []string{"exit"},
	},
	{
		Path: []string{"help"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "help"},
		Examples: []string{"help"},
	},
	{
		Path: []string{"refresh"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "refresh"},
		Examples: []string{"refresh"},
	},
	{
		Path: []string{"sync"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "refresh"},
		Examples: []string{"sync"},
	},
	{
		Path: []string{"trsf"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "trsf"},
		Examples: []string{"trsf"},
	},
	{
		// `diag` toggles; `diag on` / `diag off` are not the same request. A
		// script or a second operator saying `diag on` must not turn it OFF
		// because someone already did, which is why the positional carries the
		// word rather than the action carrying a bool.
		Path: []string{"diag"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "diag"},
		Args: []Arg{{Name: "on-off", Type: ArgString, Variadic: true, MaxCount: 1,
			Field: "Arg", OneOfArg: []string{"on", "off"}}},
		Examples: []string{"diag", "diag on", "diag off"},
	},
	{
		Path: []string{"repo"}, Surfaces: TUI,
		Action: "ScreenAction", Const: map[string]string{"Sub": "repo"},
		Args:     []Arg{{Name: "path", Type: ArgString, Field: "Arg"}},
		Examples: []string{"repo /r"},
	},

	// --- caps set / set-parent ---
	{
		Path: []string{"caps", "set"}, Surfaces: CLI | TUI,
		Notes: []string{
			"OPERATOR ONLY: re-grant a LIVE task's caps and/or scope; effective on its next request, no restart",
		},
		Action: "SetCapsAction",
		Args:   []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		// Naming neither leaves nothing to change. And --scope-for narrows
		// BELOW a base scope, so on this verb it needs --scope: unlike a
		// spawn, where the base comes from the session default, a re-grant
		// that names only the narrowing has no base to narrow. The Build
		// dropped a lone --scope-for silently; this says so.
		AtLeastOne: []Rule{{Flags: []string{"caps", "scope"}, Reason: "there is nothing to change otherwise"}},
		Requires: []Requirement{{Flags: []string{"scope-for"}, Needs: "scope",
			Reason: "a narrowing has no base to narrow unless this call names one"}},
		Flags: []Flag{
			{Name: "caps", Type: FlagString, Default: "", Field: "Caps",
				FieldType: "*protocol.Capability", Convert: "parseCapsFlag",
				// No PresenceField here, unlike the spawn verbs: Caps is a
				// POINTER, so nil already says the operator named nothing.
				// SetCapsOpts has no presence bit for it to feed, and a second
				// way to say one thing is a second thing to keep in step.
				Help: "new capability set (same syntax as --caps on submit); omitted = keep the task's current caps"},
			{Name: "scope", Type: FlagString, Default: "", Field: "Scope",
				FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag",
				Help: "new scope; omitted = keep the task's current scope"},
			{Name: "scope-for", Type: FlagString, Custom: scopeForValue, Field: "Overrides",
				FieldType: "[]protocol.ScopeOverride", Convert: "parseScopeForList",
				Help: "narrow ONE capability below --scope (written with --scope; they are one half of the authority)"},
			{Name: "cascade", Type: FlagBool, Default: false, WidensIfUnset: true, Field: "Cascade",
				Help: "also clamp every descendant to the new authority — without this a revoked task can still act through a child it spawned while it was wider"},
			{Name: "keep-conns", Type: FlagBool, Default: false, Field: "KeepConns",
				Help: "on a narrowing, leave the affected tasks' connections open"},
		},
		Examples: []string{"caps set aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --caps spawn,file_read"},
	},
	{
		// The session's SPAWN DEFAULTS: what a submit / interactive / session
		// new on this surface carries when its own line names no --caps or
		// --scope. Not a request to the server -- nothing is sent -- which is
		// why there is no task id and why the CLI does not have it: a CLI
		// invocation is one command, so there is no later command to default.
		//
		// It was `caps <mask>` and `scope <mask>` in the TUI, two grammars
		// this surface alone had, with the mask joined out of the raw tokens.
		// The flags are `caps set`'s, minus the id: the two verbs set the same
		// three things, and reading them side by side is how a reader sees
		// that one takes effect now and the other at the next spawn.
		//
		// The WebUI holds the same state as the chips in the compose panel
		// (spawnCaps / spawnScope), so this is a second door onto one value,
		// not a second value.
		Path: []string{"caps", "set-defaults"}, Surfaces: TUI | WebUI,
		Action: "SetDefaultsAction",
		// No AtLeastOne, unlike `caps set`: naming nothing is a question --
		// show the current defaults (the TUI opens the picker on it) -- where
		// on a re-grant it would be a no-op request against a live task.
		Requires: []Requirement{{Flags: []string{"scope-for"}, Needs: "scope",
			Reason: "a narrowing has no base to narrow unless this call names one"}},
		Flags: []Flag{
			{Name: "caps", Type: FlagString, Default: "", Field: "Caps",
				FieldType: "*protocol.Capability", Convert: "parseCapsFlag",
				Help: "capability set future spawns carry by default; omitted = leave it as it is"},
			{Name: "scope", Type: FlagString, Default: "", Field: "Scope",
				FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag",
				Help: "scope future spawns carry by default; omitted = leave it as it is"},
			{Name: "scope-for", Type: FlagString, Custom: scopeForValue, Field: "Overrides",
				FieldType: "[]protocol.ScopeOverride", Convert: "parseScopeForList",
				Help: "narrow ONE capability below --scope in that default (written with --scope)"},
		},
		Examples: []string{"caps set-defaults", "caps set-defaults --caps spawn,file_read",
			"caps set-defaults --scope subtree"},
	},
	{
		// `scope` is the same verb under a shorter name, the way `exit` is
		// `quit`: same Action, no Const on either, so the generator collapses
		// them onto ONE handler method rather than minting a second one
		// nothing calls.
		Path: []string{"scope"}, Surfaces: TUI | WebUI,
		Action: "SetDefaultsAction",
		Requires: []Requirement{{Flags: []string{"scope-for"}, Needs: "scope",
			Reason: "a narrowing has no base to narrow unless this call names one"}},
		Flags: []Flag{
			{Name: "caps", Type: FlagString, Default: "", Field: "Caps",
				FieldType: "*protocol.Capability", Convert: "parseCapsFlag",
				Help: "capability set future spawns carry by default; omitted = leave it as it is"},
			{Name: "scope", Type: FlagString, Default: "", Field: "Scope",
				FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag",
				Help: "scope future spawns carry by default; omitted = leave it as it is"},
			{Name: "scope-for", Type: FlagString, Custom: scopeForValue, Field: "Overrides",
				FieldType: "[]protocol.ScopeOverride", Convert: "parseScopeForList",
				Help: "narrow ONE capability below --scope in that default (written with --scope)"},
		},
		Examples: []string{"scope", "scope --scope none"},
	},
	{
		Path: []string{"caps", "set-parent"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"OPERATOR ONLY: re-point a LIVE task's parent link \u2014 the edge subtree scopes walk. --none detaches it to the operator root; --swap inverts it with its current parent. Caps and scope are untouched",
		},
		Action: "SetParentAction",
		Args:   []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		// The three name one destination. Two of them name two, which is not
		// a narrowing but a contradiction, and none of them names nothing to
		// do.
		ExactlyOne: []Rule{{Flags: []string{"parent", "none", "swap"}}},
		// ExactlyOne is decided on PRESENCE, so `--parent ""` counts as
		// having picked --parent -- and every consumer then reads an empty
		// ParentID as "detach to the operator root", which is --none. A typo
		// moved the task somewhere nobody asked for, silently and with exit 0.
		// Refused at the value, which is the only place that can see it: a
		// consumer-side `if !None && !Swap` guard cannot, because by then the
		// presence rule has already guaranteed that branch.
		Validate: func(b Bound) error {
			if b.Set["parent"] && b.Str("parent") == "" {
				return fmt.Errorf("caps set-parent: --parent needs a task id; " +
					"pass --none to detach to the operator root")
			}
			return nil
		},
		Flags: []Flag{
			{Name: "parent", Type: FlagString, Default: "", Field: "ParentID",
				Help: "new parent task id (32 hex); the target and its whole subtree move under it"},
			{Name: "none", Type: FlagBool, Default: false, Field: "None",
				Help: "detach the task to the operator root"},
			{Name: "swap", Type: FlagBool, Default: false, Field: "Swap",
				Help: "invert the task with its CURRENT parent"},
		},
		Examples: []string{"caps set-parent aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --none"},
	},

	// --- single-task session verbs ---
	{
		Path: []string{"session", "attach"}, Surfaces: CLI | TUI,
		Notes: []string{
			"reattach to a detached/running session",
		},
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "attach"},
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags:    []Flag{{Name: "view", Type: FlagBool, Default: false, Field: "View", Help: "attach in view-only mode"}},
		Examples: []string{"session attach aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "ls"}, Surfaces: CLI | TUI,
		Notes: []string{
			"JSON Lines: interactive sessions only",
		},
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "ls"},
		Examples: []string{"session ls"},
	},
	{
		Path: []string{"session", "kill"}, Surfaces: CLI | TUI,
		Notes: []string{
			"cancel a session (alias of cancel)",
		},
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "kill"},
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Examples: []string{"session kill aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "await-idle"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"one-shot: fire when the session's PTY output goes quiescent.",
			"default long-polls; --notify/--topic arm a server-side sink and return",
		},
		Action: "SessionAction",
		Const:  map[string]string{"Sub": "await-idle"},
		// Two sinks for one fire: the reply long-poll, the notification
		// egress, or an agentboard publish. Naming two asks for two.
		Exclusive: []Rule{{Flags: []string{"notify", "topic"}}},
		Args:      []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags: []Flag{
			{Name: "threshold-ms", Type: FlagUint, Default: uint(0), Field: "ThresholdMs", Help: "quiescence threshold in ms (0 = server default)"},
			{Name: "notify", Type: FlagBool, Default: false, Field: "Notify", Help: "fire via the operator-notification egress"},
			{Name: "topic", Type: FlagString, Default: "", Field: "Topic", Help: "fire via an agentboard publish to this topic"},
		},
		Examples: []string{"session await-idle aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "resize"}, Surfaces: CLI,
		Notes: []string{
			"set a live session's PTY size; the server echoing the new size back IS the acknowledgement",
		},
		Action: "SessionAction",
		Const:  map[string]string{"Sub": "resize"},
		Args:   []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags: []Flag{
			{Name: "size", Type: FlagString, Default: "", Field: "Size", Help: "new PTY size as ROWSxCOLS (e.g. 40x150)"},
			{Name: "wait-ms", Type: FlagUint, Default: uint(2000), Field: "WaitMs",
				Help: "ms to wait for the server to echo the new size back — that echo is the acknowledgement"},
			{Name: "quiet", Type: FlagBool, Default: false, Field: "Quiet", Help: "suppress the one-line result on stderr"},
		},
		Examples: []string{"session resize aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --size 40x150"},
	},
	{
		// CLI only. It renders a session's screen to STDOUT for something that
		// reads text -- an agent driving a loop, a script. The TUI has its own
		// session viewer and the WebUI a live preview panel, so neither has a
		// route for a text render, and neither ever grew one: declared for all
		// three, it was reachable on one, and the TUI's help was made to
		// advertise a line its cmdline refuses.
		Path: []string{"session", "snapshot"}, Surfaces: CLI,
		Notes: []string{
			"print the session's current PTY screen as text (view attach; non-intrusive, works without a TTY)",
			"--style/--color append attribute/color spans; --json emits {rows,cols,title,lines[],spans[]} instead of text",
			"--detect judges the state: working / blocked (waiting on a HUMAN) / idle / unknown, naming the rule and the text it read",
			"--raw writes the verbatim PTY bytes instead of the VT render (not combinable with --style/--color/--json/--detect)",
		},
		Action: "SessionAction",
		Const:  map[string]string{"Sub": "snapshot"},
		// --raw is the verbatim byte stream, so the renderers have nothing to
		// act on: combining them asks for two different outputs at once.
		Exclusive: []Rule{
			{Flags: []string{"raw", "style"}}, {Flags: []string{"raw", "color"}},
			{Flags: []string{"raw", "json"}}, {Flags: []string{"raw", "detect"}},
			{Flags: []string{"ansi", "raw"},
				Reason: "--ansi re-emits the final screen; --raw emits the whole replay verbatim"},
			{Flags: []string{"ansi", "json"},
				Reason: "--json encodes the render for a reader that parses; --ansi paints it for one that looks"},
		},
		Requires: []Requirement{{Flags: []string{"detect-agent"}, Needs: "detect",
			Reason: "there is nothing to judge by without a judgement"}},
		Args: []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags: []Flag{
			{Name: "rows", Type: FlagUint, Default: uint(40), Field: "Rows", Help: "fallback rows when the session reports no size"},
			{Name: "cols", Type: FlagUint, Default: uint(120), Field: "Cols", Help: "fallback cols when the session reports no size"},
			{Name: "settle-ms", Type: FlagUint, Default: uint(1500), Field: "SettleMs", Help: "ms to collect output before rendering"},
			{Name: "style", Type: FlagBool, Default: false, Field: "Style", Help: "also print attribute spans"},
			{Name: "color", Type: FlagBool, Default: false, Field: "Color", Help: "also print fg/bg colour spans"},
			{Name: "without-synth", Type: FlagBool, Default: false, Field: "WithoutSynth",
				Help: "render only what the PTY produced, dropping the server's replay additions"},
			{Name: "raw", Type: FlagBool, Default: false, Field: "Raw",
				Help: "write the verbatim replay bytes instead of the VT-rendered screen"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "emit the screen as one JSON object"},
			{Name: "ansi", Type: FlagBool, Default: false, Field: "ANSI", Help: "re-emit the screen WITH its colours and attributes"},
			{Name: "detect", Type: FlagBool, Default: false, Field: "Detect",
				Help: "also judge what STATE the screen shows (working / blocked / idle / unknown)"},
			{Name: "detect-agent", Type: FlagString, Default: "claude", Field: "DetectAgent", Help: "with --detect: which agent's rule set"},
		},
		Examples: []string{"session snapshot aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "stream", "attach"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"follow an event-stream session's events",
		},
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "stream-attach"},
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Examples: []string{"session stream attach aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "stream", "interrupt"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"abandon the running TURN; the agent survives to take the next one",
		},
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "stream-interrupt"},
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags:    []Flag{{Name: "flush-ms", Type: FlagUint, Default: uint(400), Field: "FlushMs", Help: "ms to let the line drain"}},
		Examples: []string{"session stream interrupt aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "stream", "finish"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"close the agent's stdin so it completes the turn in flight and exits 0",
		},
		Action:   "SessionAction",
		Const:    map[string]string{"Sub": "stream-finish"},
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID, Field: "TaskID"}},
		Flags:    []Flag{{Name: "flush-ms", Type: FlagUint, Default: uint(400), Field: "FlushMs", Help: "ms to let the line drain"}},
		Examples: []string{"session stream finish aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	},
	{
		Path: []string{"session", "stream", "approve"}, Surfaces: CLI | TUI | WebUI,
		Notes: []string{
			"answer one pending tool request. The request id is the staleness guard: an answer aimed at a request that has gone is REFUSED, not applied to whatever is pending now",
			"--message is the DENY reason and reaches the AGENT verbatim as a failed tool result; --suggestion accepts the request's Nth suggestion (a STANDING change, so it rides either verdict)",
		},
		Action: "SessionAction",
		Const:  map[string]string{"Sub": "stream-approve"},
		// The verdict is the whole point of the verb, so neither omitting it
		// nor giving both is an answer.
		ExactlyOne: []Rule{{Flags: []string{"allow", "deny"}}},
		// --message is the DENY reason. On an allow the agent gets no text at
		// all, so naming one is a mistyped verdict, not a note.
		Exclusive: []Rule{{Flags: []string{"allow", "message"},
			Reason: "--message is the deny reason; an allow carries none"}},
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Field: "TaskID"},
			{Name: "request-id", Type: ArgString, Field: "RequestID"},
		},
		Flags: []Flag{
			{Name: "allow", Type: FlagBool, Default: false, Field: "Allow", Help: "run the tool as requested"},
			{Name: "deny", Type: FlagBool, Default: false, Field: "Deny", Help: "refuse it"},
			{Name: "message", Type: FlagString, Default: "", Field: "Message",
				Help: "with --deny, the reason. It reaches the AGENT verbatim as a failed tool result"},
			// An INDEX into the request's suggestions, not a payload. Its zero
			// is the first suggestion, so presence is what says whether the
			// operator picked one -- the pre-migration flag used -1 for that
			// and this is the same distinction spelled in the declaration.
			{Name: "suggestion", Type: FlagUint, Default: uint(0), Field: "Suggestion",
				PresenceField: "SuggestionSet",
				Help:          "accept the request's Nth suggestion (0-based) as well; a suggestion is a STANDING change (e.g. stop asking for this tool), not an answer to this one call"},
			{Name: "flush-ms", Type: FlagUint, Default: uint(400), Field: "FlushMs", Help: "ms to let the line drain"},
		},
		Examples: []string{"session stream approve aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa req-1 --allow"},
	},

	// --- the agent-runtime verbs ---
	//
	// CLI-only by construction: they are called from inside a task's Bash tool
	// and read HARNESS_* env, which no operator surface has.
	{
		Path: []string{"agent", "inbox"}, Surfaces: CLI,
		Notes: []string{
			"idempotent dump of subscribed topics; --since 0 (default) = the whole ring",
		},
		Action: "AgentAction",
		Const:  map[string]string{"Sub": "inbox"},
		Flags: append(agentCommonFlags(),
			Flag{Name: "since", Type: FlagUint64, Default: uint64(0), Field: "Since", Help: "start from this seq (0 = whole ring)"},
			Flag{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "JSON Lines instead of text"},
			Flag{Name: "user-prompt-submit-hook", Type: FlagBool, Default: false, Field: "UserPromptSubmitHook",
				Help: "hook mode: ask for what has not been injected, and mark it injected in the same operation"},
			Flag{Name: "in-reply-to", Type: FlagUint64, Default: uint64(0), Field: "InReplyTo", Help: "only messages replying to this seq"},
		),
		Examples: []string{"agent inbox", "agent inbox --json"},
	},
	{
		Path: []string{"agent", "wait"}, Surfaces: CLI,
		Notes: []string{
			"take everything after --since, blocking only if there is nothing;",
			"omitting --since means cursor 0, so a non-empty ring returns AT ONCE with old messages",
			"(scripting; NOT from an agent turn)",
		},
		Action: "AgentAction",
		Const:  map[string]string{"Sub": "wait"},
		Flags: append(agentCommonFlags(),
			Flag{Name: "topic", Type: FlagString, Default: "", Field: "Topic", Help: "topic to wait on"},
			Flag{Name: "since", Type: FlagUint64, Default: uint64(0), Field: "Since", Help: "take everything after this seq"},
			Flag{Name: "in-reply-to", Type: FlagUint64, Default: uint64(0), Field: "InReplyTo", Help: "only messages replying to this seq"},
			Flag{Name: "timeout", Type: FlagDuration, Default: 5 * time.Minute, Field: "Timeout",
				Help: "max block duration"},
		),
		Examples: []string{"agent wait --topic chat.abcd1234"},
	},
	{
		Path: []string{"agent", "subscribe"}, Surfaces: CLI,
		Notes: []string{
			"register a subscription",
		},
		Action: "AgentAction",
		Const:  map[string]string{"Sub": "subscribe"},
		// --self names the agent's own id-directed topic, so pairing it with
		// --topic asks for two destinations at once.
		Exclusive: []Rule{{Flags: []string{"self", "topic"}}},
		Flags:     append(agentCommonFlags(), agentTopicSelfFlags()...),
		Examples:  []string{"agent subscribe --topic chat.abcd1234"},
	},
	{
		Path: []string{"agent", "unsubscribe"}, Surfaces: CLI,
		Notes: []string{
			"remove a subscription",
		},
		Action: "AgentAction",
		Const:  map[string]string{"Sub": "unsubscribe"},
		// --self names the agent's own id-directed topic, so pairing it with
		// --topic asks for two destinations at once.
		Exclusive: []Rule{{Flags: []string{"self", "topic"}}},
		Flags:     append(agentCommonFlags(), agentTopicSelfFlags()...),
		Examples:  []string{"agent unsubscribe --topic chat.abcd1234"},
	},
	{
		Path: []string{"agent", "topics"}, Surfaces: CLI,
		Notes: []string{
			"list every topic on the board (JSON Lines) (cap: board_observe)",
		},
		Action:   "AgentAction",
		Const:    map[string]string{"Sub": "topics"},
		Flags:    agentCommonFlags(),
		Examples: []string{"agent topics"},
	},
	{
		Path: []string{"agent", "subscriptions"}, Surfaces: CLI,
		Notes: []string{
			"list this agent's registered patterns (JSON Lines)",
		},
		Action:   "AgentAction",
		Const:    map[string]string{"Sub": "subscriptions"},
		Flags:    agentCommonFlags(),
		Examples: []string{"agent subscriptions"},
	},
	{
		Path: []string{"agent", "retained"}, Surfaces: CLI,
		Notes: []string{
			"list a topic's retained ring as metadata only, no payload (no cap)",
		},
		Action: "AgentAction",
		Const:  map[string]string{"Sub": "retained"},
		// --self names the agent's own id-directed topic, so pairing it with
		// --topic asks for two destinations at once.
		Exclusive: []Rule{{Flags: []string{"self", "topic"}}},
		Flags:     append(agentCommonFlags(), agentTopicSelfFlags()...),
		Examples:  []string{"agent retained --self"},
	},
	{
		Path: []string{"agent", "purge"}, Surfaces: CLI,
		Notes: []string{
			"drop a topic's retained buffer, or one message by seq (cap: purge)",
		},
		Action: "AgentAction",
		Const:  map[string]string{"Sub": "purge"},
		// --self names the agent's own id-directed topic, so pairing it with
		// --topic asks for two destinations at once.
		Exclusive: []Rule{{Flags: []string{"self", "topic"}}},
		Flags: append(append(agentCommonFlags(), agentTopicSelfFlags()...),
			Flag{Name: "seq", Type: FlagUint64, Default: uint64(0), Field: "Seq", WidensIfUnset: true,
				Help: "drop one message by seq; omitted drops the topic's retained buffer"},
		),
		Examples: []string{"agent purge --self", "agent purge --topic chat.abcd1234 --seq 42"},
	},
	{
		Path: []string{"agent", "read"}, Surfaces: CLI,
		Notes: []string{
			"fetch one retained message, whole; the hooks name it when they decline to inline a large body.",
			"Limited to topics this task subscribes to.",
		},
		Action:   "AgentAction",
		Const:    map[string]string{"Sub": "read"},
		Args:     []Arg{{Name: "seq", Type: ArgUint, Field: "Seq"}},
		Flags:    agentCommonFlags(),
		Examples: []string{"agent read 42"},
	},
	{
		Path: []string{"agent", "retract"}, Surfaces: CLI,
		Notes: []string{
			"withdraw a message YOU sent: gone from every agent path, still visible to the operator as retracted",
			"(no cap; authorship-checked). A reply to a message addressed to you retracts it automatically;",
			"send --no-retire-on-reply to keep one alive past its answer.",
		},
		Action:   "AgentAction",
		Const:    map[string]string{"Sub": "retract"},
		Args:     []Arg{{Name: "seq", Type: ArgUint, Field: "Seq"}},
		Flags:    agentCommonFlags(),
		Examples: []string{"agent retract 42"},
	},
}

// Lookup finds the spec for a verb path.
//
// A verb with no hand-written Build gets the generated one, keyed by path.
// Wiring it here rather than in the table literal is what keeps table.go free
// of generated names -- so the package still compiles when actions_gen.go is
// missing, which is the only way the generator can be run to produce it.
// MaxPathLen is the longest verb path in the table -- `session stream turn` and
// its siblings are three words. A lookup that walks candidate prefixes must
// start here, not at a literal 2: the WebUI bridge hard-coded 2 and left every
// three-word path unreachable.
var MaxPathLen = func() int {
	n := 0
	for _, v := range Verbs {
		if len(v.Path) > n {
			n = len(v.Path)
		}
	}
	return n
}()

func Lookup(path ...string) (VerbSpec, bool) {
	for _, v := range Verbs {
		if len(v.Path) != len(path) {
			continue
		}
		match := true
		for i := range path {
			if v.Path[i] != path[i] {
				match = false
				break
			}
		}
		if match {
			return v, true
		}
	}
	return VerbSpec{}, false
}

// PathsForSurface lists the verb paths declared for one surface, so a surface
// can assert its dispatch covers all of them.
func PathsForSurface(s Surface) []string {
	var out []string
	for _, v := range Verbs {
		if v.Surfaces.Has(s) {
			out = append(out, v.FlagSetName())
		}
	}
	return out
}

// parseUintArgs converts positional ids, naming the verb and what the number
// means so a typo says which argument it was.
func parseUintArgs(verbName, what string, args []string) ([]uint64, error) {
	out := make([]uint64, 0, len(args))
	for _, raw := range args {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: bad %s %q", verbName, what, raw)
		}
		out = append(out, n)
	}
	return out, nil
}

// uintFlag reads a uint flag, returning 0 when it is absent.
func uintFlag(b Bound, name string) uint {
	v, _ := b.Flags[name].(uint)
	return v
}

// agentSendFlags is the flag set shared by `agent send` and `agent dispatch`.
// dispatch adds a timeout because it blocks for the reply.
func agentSendFlags(withTimeout bool) []Flag {
	out := []Flag{
		{Name: "server-cid", Type: FlagString, Default: "", Field: "ServerCID",
			Help: "server ConnectionID (env: HARNESS_SERVER_CID)"},
		{Name: "topic", Type: FlagString, Default: "", Field: "Topic", Help: "agentboard topic"},
		// The payload's SOURCE, not its bytes: "-" is a VALUE of --data, never
		// a positional, and reading stdin belongs to the caller that owns it.
		// DataSet rather than Data != "" because the default IS "-", so the
		// value alone cannot say whether the operator chose it.
		{Name: "data", Type: FlagString, Default: "-", Field: "Data", PresenceField: "DataSet",
			Help: `payload string, or "-" to read stdin`},
		{Name: "reply-to", Type: FlagString, Default: "", Field: "ReplyTo",
			Help: "route replies to THIS message to this topic instead of your own chat.<short-id>"},
	}
	if withTimeout {
		return append(out, Flag{Name: "timeout", Type: FlagDuration, Default: 5 * time.Minute, Field: "Timeout",
			Help: "max wait for the whole call (publish ack + reply)"})
	}
	return append(out,
		Flag{Name: "in-reply-to", Type: FlagUint64, Default: uint64(0), Field: "InReplyTo",
			Help: "seq of the message being replied to; with it, --topic may be omitted"},
		Flag{Name: "no-retire-on-reply", Type: FlagBool, Default: false, Field: "NoRetireOnReply",
			Help: "keep this message on the board even after its recipient replies"},
	)
}

// agentCommonFlags is the one flag every agent verb carries. --server-cid is
// env-primary (HARNESS_SERVER_CID); the auth ticket is env-ONLY and has no
// flag at all, on purpose.
func agentCommonFlags() []Flag {
	return []Flag{{
		Name: "server-cid", Type: FlagString, Default: "", Field: "ServerCID",
		Help:    "server ConnectionID (env: HARNESS_SERVER_CID)",
		Resolve: []Tier{{Env: "HARNESS_SERVER_CID"}},
	}}
}

// agentTopicSelfFlags is the --topic / --self pair: name a topic, or the
// agent's own id-directed one.
func agentTopicSelfFlags() []Flag {
	return []Flag{
		{Name: "topic", Type: FlagString, Default: "", Field: "Topic", Help: "agentboard topic"},
		{Name: "self", Type: FlagBool, Default: false, Field: "Self", Help: "this agent's own chat.<short-id> topic"},
	}
}

// Family-wide prose: what the whole first word is for, said once. See
// FamilyNotes for why this is not repeated onto each path.
func init() {
	FamilyNotes["git"] = []string{
		"read-only git view of a task's worktree (requires file_read)",
		"runs in the worktree while the task lives, and against the retained",
		"harness/<task-id> branch after it ends (committed work only)",
		"--subrepo DIR runs any of them inside a nested repository (a plain",
		"nested repo is invisible from outside it); subrepos lists them",
	}
	FamilyNotes["workspace"] = []string{
		"the TUI applies a workspace on start, on reconnect, and on `workspace apply`,",
		"and `workspace detach [--stop]` there stops it re-applying;",
		"neither exists here — a forward dies with the process that holds it",
	}
	FamilyNotes["agent"] = []string{
		"agent-to-agent message ops, run from inside a task's own shell.",
		"Env-primary (HARNESS_*): SERVER_CID, TASK_ID, RUNNER_ID, HOSTNAME, WS_PATH, REPO_PATH.",
		"HARNESS_AUTH_TICKET is env-only -- no flag accepts it, so an agent cannot be told to present someone else's.",
	}
	FamilyNotes["server"] = []string{
		"server-side operations, addressed to the server rather than to a task",
	}
	FamilyNotes["board"] = []string{
		"inspect/withdraw/purge the agentboard (cap: board_observe; retract and purge: purge)",
	}
}
