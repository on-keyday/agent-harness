package verb

import (
	"fmt"
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
		Path:     []string{"prune"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			// Variadic and optional: no ids means time mode, which is the
			// difference between "forget old terminal tasks" and "forget
			// exactly these".
			{Name: "task-id", Type: ArgTaskID, Variadic: true},
		},
		Flags: []Flag{
			{
				Name: "before", Type: FlagDuration, Default: 7 * 24 * time.Hour,
				Help: "forget terminal tasks older than this (ignored when TASK_IDs are passed)",
			},
			{
				Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false,
				Help: "with TASK_IDs: also forget tasks the server still considers active (Queued/Running/Detached)",
			},
		},
		Examples: []string{
			"prune",
			"prune --before 24h",
			"prune --force aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			// The flag AFTER the ids, which is the form stdlib parsing drops
			// and the reason this verb permutes.
			"prune aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --force",
		},
		Build: func(b Bound) (Action, error) {
			d, _ := b.Flags["before"].(time.Duration)
			return PruneAction{
				Before:  d,
				TaskIDs: b.Args,
				Force:   b.Bool("force"),
			}, nil
		},
	},
	// --- file ---
	//
	// Seven sub-verbs, and the family where per-surface narrowing first bites:
	// a browser has no local path, so `file push` names two positionals there
	// and three everywhere else. Declared with Arg.Surfaces rather than as a
	// separate verb, because it is one operation reached from three places.
	{
		Path:     []string{"file", "push"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			{
				Name: "local-src", Type: ArgString,
				Surfaces:      CLI | TUI,
				SurfaceReason: "a browser has no local path to name; the WebUI supplies the bytes from a file picker",
			},
			{Name: "worktree-rel-dst", Type: ArgString},
		},
		Flags: []Flag{
			{Name: "recursive", Aliases: []string{"r"}, Type: FlagBool, Default: false,
				Help: "transfer a directory tree"},
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false,
				Help: "overwrite existing destination"},
			{Name: "parents", Aliases: []string{"p"}, Type: FlagBool, Default: false,
				Help: "create missing parent directories of the destination (mkdir -p)"},
		},
		Examples: []string{
			"file push aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ./local.txt docs/local.txt",
			"file push -r -f aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ./dir docs/dir",
		},
		Build: func(b Bound) (Action, error) {
			a := FilePushAction{Recursive: b.Bool("recursive"), Force: b.Bool("force"), Parents: b.Bool("parents")}
			a.TaskID = b.Args[0]
			if len(b.Args) == 3 {
				a.LocalSrc, a.RemoteDst = b.Args[1], b.Args[2]
			} else {
				a.RemoteDst = b.Args[1]
			}
			return a, nil
		},
	},
	{
		Path:     []string{"file", "pull"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			{Name: "worktree-rel-src", Type: ArgString},
			{
				Name: "local-dst", Type: ArgString,
				Surfaces:      CLI | TUI,
				SurfaceReason: "a browser downloads the file rather than writing it to a path it names",
			},
		},
		Flags: []Flag{
			{Name: "recursive", Aliases: []string{"r"}, Type: FlagBool, Default: false,
				Help: "transfer a directory tree"},
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false,
				Help: "overwrite existing destination"},
			// -o / -n existed only in the TUI before the migration. Adding them
			// to the other surfaces widens what parses and never narrows it.
			{Name: "offset", Aliases: []string{"o"}, Type: FlagUint64, Default: uint64(0),
				Help: "first byte to pull (single-file pull only)"},
			{Name: "length", Aliases: []string{"n"}, Type: FlagUint64, Default: uint64(0),
				Help: "max bytes to pull; 0 = to end of file"},
		},
		Examples: []string{
			"file pull aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/x.txt ./x.txt",
			"file pull -r aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs ./docs",
		},
		Build: func(b Bound) (Action, error) {
			off, _ := b.Flags["offset"].(uint64)
			ln, _ := b.Flags["length"].(uint64)
			// Cross-flag validity belongs in Build, which every surface goes
			// through -- the TUI used to refuse this at parse time and the CLI
			// after it, which is two places to keep in step. A directory pull
			// is a generated tar, whose byte offsets are not a stable thing to
			// index into.
			if b.Bool("recursive") && (off != 0 || ln != 0) {
				return nil, fmt.Errorf("file pull: --offset/--length cannot be combined with --recursive")
			}
			a := FilePullAction{Recursive: b.Bool("recursive"), Force: b.Bool("force"), Offset: off, Length: ln}
			a.TaskID, a.RemoteSrc = b.Args[0], b.Args[1]
			if len(b.Args) == 3 {
				a.LocalDst = b.Args[2]
			}
			return a, nil
		},
	},
	{
		Path:     []string{"file", "ls"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			// Variadic rather than "optional": the declaration has one way to
			// say "zero or more", and the Build takes at most the first.
			{Name: "worktree-rel-dir", Type: ArgString, Variadic: true},
		},
		Examples: []string{
			"file ls aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"file ls aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs",
		},
		Build: func(b Bound) (Action, error) {
			a := FileLsAction{TaskID: b.Args[0]}
			if len(b.Args) > 2 {
				return nil, fmt.Errorf("file ls: takes at most one directory")
			}
			if len(b.Args) == 2 {
				a.RelPath = b.Args[1]
			}
			return a, nil
		},
	},
	{
		Path:     []string{"file", "mkdir"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			{Name: "worktree-rel-dir", Type: ArgString},
		},
		Flags: []Flag{
			{Name: "parents", Aliases: []string{"p"}, Type: FlagBool, Default: false,
				Help: "create missing parent directories (mkdir -p); also makes an existing directory a success"},
		},
		Examples: []string{"file mkdir -p aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/sub"},
		Build: func(b Bound) (Action, error) {
			return FileMkdirAction{TaskID: b.Args[0], RelPath: b.Args[1], Parents: b.Bool("parents")}, nil
		},
	},
	{
		Path:     []string{"file", "delete"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			{Name: "worktree-rel-path", Type: ArgString},
		},
		Flags: []Flag{
			{Name: "recursive", Aliases: []string{"r"}, Type: FlagBool, Default: false,
				Help: "target a directory tree instead of a single file (uses dir_delete)"},
			// Without -r this flag is ignored, so its absence never widens: -r
			// alone refuses a non-empty directory rather than emptying it.
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false,
				Help: "with -r: delete non-empty directory contents recursively (RemoveAll). Ignored without -r"},
		},
		Examples: []string{
			"file delete aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/x.txt",
			"file delete -r -f aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs",
		},
		Build: func(b Bound) (Action, error) {
			return FileDeleteAction{TaskID: b.Args[0], RelPath: b.Args[1],
				Recursive: b.Bool("recursive"), Force: b.Bool("force")}, nil
		},
	},
	{
		Path:     []string{"file", "edit"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			{Name: "worktree-rel-path", Type: ArgString},
		},
		Examples: []string{"file edit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/x.txt"},
		Build: func(b Bound) (Action, error) {
			return FileEditAction{TaskID: b.Args[0], RelPath: b.Args[1]}, nil
		},
	},
	{
		Path:     []string{"file", "new"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID},
			{Name: "worktree-rel-path", Type: ArgString},
		},
		Examples: []string{"file new aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa docs/new.txt"},
		Build: func(b Bound) (Action, error) {
			return FileNewAction{TaskID: b.Args[0], RelPath: b.Args[1]}, nil
		},
	},
	// --- git ---
	//
	// The path is `git <sub>`, but the task id sits BETWEEN them on the command
	// line (`git <task-id> diff`), so callers peel the id and the pathspec
	// before handing the rest here. Sub-verbs differ in how many revisions they
	// take, which is why each is its own entry rather than one `git` verb with
	// a mode flag.
	//
	// --max-bytes was missing from the TUI before the migration, so a large
	// diff could not be capped there; declaring it once gives it to every
	// surface. The WebUI accepted it and threw it away, which was worse than
	// refusing it.
	{
		Path:     []string{"git", "log"},
		Surfaces: CLI | TUI | WebUI,
		Pathspec: true,
		Args:     []Arg{{Name: "revision", Type: ArgString, Variadic: true}},
		Flags: []Flag{
			{Name: "max", Type: FlagUint, Default: uint(0), Help: "maximum commits (0 = 100, capped at 1000)"},
			{Name: "subrepo", Type: FlagString, Default: "", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git log", "git log --max 20"},
		Build: func(b Bound) (Action, error) {
			if len(b.Args) > 1 {
				return nil, fmt.Errorf("git log: at most one revision (got %d)", len(b.Args))
			}
			a := GitAction{Sub: "log", Subrepo: b.Str("subrepo"), Path: b.Pathspec}
			if m, ok := b.Flags["max"].(uint); ok {
				a.Max = uint32(m)
			}
			if len(b.Args) == 1 {
				a.BaseRev = b.Args[0]
			}
			return a, nil
		},
	},
	{
		Path:     []string{"git", "diff"},
		Surfaces: CLI | TUI | WebUI,
		Pathspec: true,
		Args:     []Arg{{Name: "revision", Type: ArgString, Variadic: true}},
		Flags: []Flag{
			// A long-to-long alias: --cached is git's own spelling of --staged,
			// bound to one variable, unlike session send's --enter and -e.
			{Name: "staged", Aliases: []string{"cached"}, Type: FlagBool, Default: false,
				Help: "diff the index instead of the working tree"},
			{Name: "submodule", Type: FlagBool, Default: false,
				Help: "inline a submodule's own file-level changes (the output is then not an applyable patch)"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Help: "maximum diff bytes (0 = 2MiB, capped at 8MiB)"},
			{Name: "subrepo", Type: FlagString, Default: "", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git diff", "git diff --staged", "git diff HEAD~1 HEAD"},
		Build: func(b Bound) (Action, error) {
			a := GitAction{Sub: "diff", Staged: b.Bool("staged"), Submodule: b.Bool("submodule"), Subrepo: b.Str("subrepo"), Path: b.Pathspec}
			if m, ok := b.Flags["max-bytes"].(uint); ok {
				a.MaxBytes = uint32(m)
			}
			// Counted the way git counts: none = unstaged, one = that revision
			// against the working tree, two = commit against commit.
			switch len(b.Args) {
			case 0:
			case 1:
				a.BaseRev = b.Args[0]
			case 2:
				if a.Staged {
					return nil, fmt.Errorf("git diff: --staged names the index as the right-hand side, so a second revision has nowhere to go")
				}
				a.BaseRev, a.TargetRev = b.Args[0], b.Args[1]
			default:
				return nil, fmt.Errorf("git diff: at most two revisions (got %d)", len(b.Args))
			}
			return a, nil
		},
	},
	{
		Path:     []string{"git", "show"},
		Surfaces: CLI | TUI | WebUI,
		Pathspec: true,
		Args:     []Arg{{Name: "revision", Type: ArgString, Variadic: true}},
		Flags: []Flag{
			{Name: "submodule", Type: FlagBool, Default: false, Help: "inline a submodule's own file-level changes"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Help: "maximum bytes (0 = 2MiB, capped at 8MiB)"},
			{Name: "subrepo", Type: FlagString, Default: "", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git show", "git show HEAD"},
		Build: func(b Bound) (Action, error) {
			if len(b.Args) > 1 {
				return nil, fmt.Errorf("git show: at most one revision (got %d)", len(b.Args))
			}
			a := GitAction{Sub: "show", Submodule: b.Bool("submodule"), Subrepo: b.Str("subrepo"), Path: b.Pathspec}
			if m, ok := b.Flags["max-bytes"].(uint); ok {
				a.MaxBytes = uint32(m)
			}
			if len(b.Args) == 1 {
				a.BaseRev = b.Args[0]
			}
			return a, nil
		},
	},
	{
		Path:     []string{"git", "status"},
		Surfaces: CLI | TUI | WebUI,
		Pathspec: true,
		Flags: []Flag{
			{Name: "subrepo", Type: FlagString, Default: "", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git status"},
		Build: func(b Bound) (Action, error) {
			return GitAction{Sub: "status", Subrepo: b.Str("subrepo"), Path: b.Pathspec}, nil
		},
	},
	{
		Path:     []string{"git", "subrepos"},
		Surfaces: CLI | TUI | WebUI,
		Pathspec: true,
		Flags: []Flag{
			{Name: "subrepo", Type: FlagString, Default: "", Help: "list nested repos under this worktree-relative directory"},
		},
		Examples: []string{"git subrepos"},
		Build: func(b Bound) (Action, error) {
			return GitAction{Sub: "subrepos", Subrepo: b.Str("subrepo"), Path: b.Pathspec}, nil
		},
	},
	{
		Path:     []string{"git", "file"},
		Surfaces: CLI | TUI | WebUI,
		Pathspec: true,
		// Variadic because the path may arrive as a positional OR after `--`;
		// Build refuses both and refuses neither, which is the rule the TUI
		// enforced by hand and the CLI did not enforce at all.
		Args: []Arg{{Name: "path", Type: ArgString, Variadic: true}},
		Flags: []Flag{
			{Name: "staged", Type: FlagBool, Default: false, Help: "read the indexed copy"},
			{Name: "rev", Type: FlagString, Default: "", Help: "read the copy at this revision"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0), Help: "maximum bytes (0 = 2MiB, capped at 8MiB)"},
			{Name: "subrepo", Type: FlagString, Default: "", Help: "run the query inside this worktree-relative nested repo"},
		},
		Examples: []string{"git file README.md", "git file --rev HEAD~1 README.md"},
		Build: func(b Bound) (Action, error) {
			p := b.Pathspec
			switch {
			case len(b.Args) > 1:
				return nil, fmt.Errorf("git file: one path (got %d)", len(b.Args))
			case len(b.Args) == 1 && p != "":
				return nil, fmt.Errorf("git file: path given twice — once as an argument and once after `--`")
			case len(b.Args) == 1:
				p = b.Args[0]
			case p == "":
				return nil, fmt.Errorf("git file: a path is required, as an argument or after `--`")
			}
			a := GitAction{Sub: "file", Staged: b.Bool("staged"), TargetRev: b.Str("rev"),
				Subrepo: b.Str("subrepo"), Path: p}
			if m, ok := b.Flags["max-bytes"].(uint); ok {
				a.MaxBytes = uint32(m)
			}
			return a, nil
		},
	},
	// --- exec (exec_run) ---
	{
		Path:     []string{"exec"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "task-id", Type: ArgTaskID}},
		// The argv follows a literal `--` and stays a LIST: the runner needs
		// the word boundaries. --shell is the one case that joins, and it does
		// so because the operator asked for shell interpretation.
		Trailing: &Trailing{
			Name: "command", List: true, AfterSeparator: true,
			Reason: "everything after `--` is the argv verbatim; re-scanning it for flags is how a command whose own first word is --shell gets eaten",
		},
		Flags: []Flag{
			{Name: "shell", Type: FlagBool, Default: false,
				Help: "hand it to the RUNNER's shell as one line (sh -c / cmd /c by its platform)"},
			{Name: "sshd-parent", Type: FlagBool, Default: false,
				Help: "run under the task's sshd parent process"},
		},
		Examples: []string{"exec aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -- ls -la"},
		Build: func(b Bound) (Action, error) {
			if len(b.TrailArgs) == 0 {
				return nil, fmt.Errorf("exec: a command is required after `--`")
			}
			// `exec --shell kill 3` used to parse as "run `3` on a task named
			// kill". ls and kill are sub-verbs, and the run flags do not apply
			// to them, so naming one here is a mistake rather than a task id.
			if sub := b.Args[0]; sub == "ls" || sub == "kill" {
				return nil, fmt.Errorf("exec: %q is a sub-verb; --shell / --sshd-parent do not apply to it", sub)
			}
			if b.Bool("sshd-parent") && !b.Bool("shell") {
				return nil, fmt.Errorf("exec: --sshd-parent needs --shell; what it renames is the shell")
			}
			argv := b.TrailArgs
			if b.Bool("shell") {
				argv = []string{b.Trail}
			}
			return ExecRunAction{Sub: "run", TaskID: b.Args[0], Argv: argv,
				Shell: b.Bool("shell"), SshdParent: b.Bool("sshd-parent")}, nil
		},
	},
	{
		Path:     []string{"exec", "ls"},
		Surfaces: CLI | TUI | WebUI,
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Help: "only execs against this task id"},
			// --json was CLI-only before the migration; declaring it once gives
			// it to the surfaces that silently lacked it.
			{Name: "json", Type: FlagBool, Default: false, Help: "one JSON object per exec"},
		},
		Examples: []string{"exec ls", "exec ls --json"},
		Build: func(b Bound) (Action, error) {
			return ExecRunAction{Sub: "ls", TaskID: b.Str("task"), TaskFilter: b.Str("task"), JSON: b.Bool("json")}, nil
		},
	},
	{
		Path:     []string{"exec", "kill"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "exec-id", Type: ArgUint, Variadic: true}},
		Examples: []string{"exec kill 3"},
		Build: func(b Bound) (Action, error) {
			ids, err := parseUintArgs("exec kill", "exec id", b.Args)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				return nil, fmt.Errorf("exec kill: at least one exec id")
			}
			return ExecRunAction{Sub: "kill", ExecID: ids[0], ExecIDs: ids}, nil
		},
	},

	// --- forward ---
	//
	// Opening a forward is CLI-only: the TUI opens one from its port-forward
	// pane and a browser cannot bind a local listener at all. ls / kill / tap
	// are on all three.
	{
		Path:     []string{"forward", "ls"},
		Surfaces: CLI | TUI | WebUI,
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Help: "only forwards for this task id"},
			{Name: "json", Type: FlagBool, Default: false, Help: "one JSON object per forward"},
		},
		Examples: []string{"forward ls", "forward ls --json"},
		Build: func(b Bound) (Action, error) {
			return ForwardLsAction{TaskFilter: b.Str("task"), JSON: b.Bool("json")}, nil
		},
	},
	{
		Path:     []string{"forward", "kill"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "forward-id", Type: ArgUint, Variadic: true}},
		Examples: []string{"forward kill 7"},
		Build: func(b Bound) (Action, error) {
			ids, err := parseUintArgs("forward kill", "forward id", b.Args)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				return nil, fmt.Errorf("forward kill: at least one forward id")
			}
			return ForwardKillAction{ForwardID: ids[0], ForwardIDs: ids}, nil
		},
	},
	{
		Path:     []string{"forward", "tap"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "forward-id", Type: ArgUint}},
		Flags: []Flag{
			{Name: "dir", Type: FlagString, Default: "both", Help: "to-target, from-target or both"},
			{Name: "max-bytes", Type: FlagUint, Default: uint(0),
				Help: "cut each record's payload to this many bytes (0 = whole payload)"},
			// The four render modes are mutually exclusive; Build refuses two.
			// They were CLI-only before -- the TUI took only --dir/--max-bytes.
			{Name: "hex", Type: FlagBool, Default: false, Help: "hexdump body (default)"},
			{Name: "text", Type: FlagBool, Default: false, Help: "printable body, no offset column"},
			{Name: "raw", Type: FlagBool, Default: false, Help: "payload bytes only; requires an explicit --dir"},
			{Name: "json", Type: FlagBool, Default: false, Help: "one JSON object per record"},
		},
		Examples: []string{"forward tap 7", "forward tap 7 --dir to-target --text"},
		Build: func(b Bound) (Action, error) {
			ids, err := parseUintArgs("forward tap", "forward id", b.Args)
			if err != nil {
				return nil, err
			}
			switch b.Str("dir") {
			case "to-target", "from-target", "both":
			default:
				return nil, fmt.Errorf("forward tap: --dir %q (want to-target, from-target or both)", b.Str("dir"))
			}
			mode, n := "hex", 0
			for _, m := range []string{"hex", "text", "raw", "json"} {
				if b.Bool(m) {
					n++
					mode = m
				}
			}
			if n > 1 {
				return nil, fmt.Errorf("forward tap: --hex, --text, --raw and --json are mutually exclusive")
			}
			// --raw writes payloads with no headers, so two directions
			// concatenated onto one stdout interleave two conversations into a
			// byte soup no decoder can read.
			if mode == "raw" && b.Str("dir") == "both" {
				return nil, fmt.Errorf("forward tap: --raw needs an explicit --dir (to-target or from-target); " +
					"both directions on one stdout is not a stream any decoder can read")
			}
			a := ForwardTapAction{ForwardID: ids[0], Dir: b.Str("dir"), Mode: mode}
			if mb, ok := b.Flags["max-bytes"].(uint); ok {
				a.MaxRecordBytes = uint32(mb)
			}
			return a, nil
		},
	},

	// --- server ---
	{
		Path:     []string{"server", "dial-runner"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "runner-cid", Type: ArgString}},
		Flags: []Flag{
			{Name: "via", Type: FlagString, Default: "",
				Help: "relay through this registered runner CID (copy from `harness-cli ls`)"},
		},
		Examples: []string{"server dial-runner ws:127.0.0.1:9000-abcd"},
		Build: func(b Bound) (Action, error) {
			return ServerDialRunnerAction{RunnerCID: b.Args[0], Via: b.Str("via")}, nil
		},
	},

	// --- ssh-gateway ---
	//
	// Two shapes, not one: the CLI runs the gateway in the foreground with
	// flags, while the TUI starts and stops a background one. Declared as
	// separate paths because they are separate operations wearing one name.
	{
		Path:     []string{"ssh-gateway"},
		Surfaces: CLI,
		Flags: []Flag{
			{Name: "listen", Type: FlagString, Default: "127.0.0.1:2222",
				Help: "ssh listen host:port (no ssh auth on a loopback bind; --authorized-keys is required off loopback)"},
			{Name: "host-key", Type: FlagString, Default: "",
				Help: "ssh host key path (default: alongside the workspace config; generated on first run)"},
			{Name: "authorized-keys", Type: FlagString, Default: "",
				Help: "OpenSSH authorized_keys file; optional on a loopback bind, required otherwise"},
		},
		Examples: []string{"ssh-gateway", "ssh-gateway --listen 127.0.0.1:2223"},
		Build: func(b Bound) (Action, error) {
			return SSHGatewayAction{Listen: b.Str("listen"), HostKeyPath: b.Str("host-key"),
				AuthorizedKeys: b.Str("authorized-keys")}, nil
		},
	},

	// --- workspace ---
	{
		Path:     []string{"workspace", "save"},
		Surfaces: CLI | TUI,
		Args:     []Arg{{Name: "name", Type: ArgString}},
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Surfaces: CLI,
				SurfaceReason: "the TUI picks the tasks in a picker instead of naming one on the line",
				Help:          "record only this task (32 hex); omitted = every task the registry reports a forward for"},
			{Name: "resume", Type: FlagString, Default: "continue", Surfaces: CLI,
				SurfaceReason: "written through the TUI's picker rather than a flag",
				Help:          "no | continue | fresh — for a task block being written for the FIRST time"},
			{Name: "runner", Type: FlagString, Default: "assigned", Surfaces: CLI,
				SurfaceReason: "written through the TUI's picker rather than a flag",
				Help:          "assigned | any — for a task block being written for the FIRST time"},
			{Name: "repo", Type: FlagString, Default: "", Surfaces: CLI,
				SurfaceReason: "the TUI already knows its repo from the session",
				Help:          "repo identifier to record in the workspace"},
			{Name: "all", Type: FlagBool, Default: false, Surfaces: TUI,
				SurfaceReason: "skips the TUI's task picker; the CLI has no picker to skip",
				Help:          "write every live session without opening the picker"},
		},
		Examples: []string{"workspace save dev"},
		Build: func(b Bound) (Action, error) {
			return WorkspaceAction{Sub: "save", Name: b.Args[0], TaskID: b.Str("task"),
				Resume: b.Str("resume"), Runner: b.Str("runner"), Repo: b.Str("repo"),
				All: b.Bool("all")}, nil
		},
	},
	{
		Path:     []string{"workspace", "rm"},
		Surfaces: CLI | TUI,
		// A name is required, and there is no "the current one" shorthand:
		// deleting is the one verb here that cannot be undone by re-running it.
		Args:     []Arg{{Name: "name", Type: ArgString}},
		Examples: []string{"workspace rm dev"},
		Build: func(b Bound) (Action, error) {
			return WorkspaceAction{Sub: "rm", Name: b.Args[0]}, nil
		},
	},
	{
		Path:     []string{"workspace", "ls"},
		Surfaces: CLI | TUI,
		Examples: []string{"workspace ls"},
		Build: func(b Bound) (Action, error) {
			return WorkspaceAction{Sub: "ls"}, nil
		},
	},
	{
		Path:     []string{"workspace", "show"},
		Surfaces: CLI | TUI,
		Args:     []Arg{{Name: "name", Type: ArgString, Variadic: true}},
		Examples: []string{"workspace show", "workspace show dev"},
		Build:    buildWorkspaceOptionalName("show"),
	},
	{
		Path: []string{"workspace", "apply"},
		// TUI-only: applying establishes forwards and resumes tasks, and a
		// forward dies with the process that holds it -- so there is nothing
		// for a one-shot CLI invocation to apply. usage() says as much.
		Surfaces: TUI,
		Args:     []Arg{{Name: "name", Type: ArgString, Variadic: true}},
		Examples: []string{"workspace apply", "workspace apply dev"},
		Build:    buildWorkspaceOptionalName("apply"),
	},
	{
		Path:     []string{"workspace", "detach"},
		Surfaces: TUI,
		// Takes no name on purpose: there is only ever one installed
		// workspace, and accepting a name would invite `detach other` to read
		// as "detach that one instead of mine".
		Flags: []Flag{
			{Name: "stop", Type: FlagBool, Default: false,
				Help: "also stop what the workspace started"},
		},
		Examples: []string{"workspace detach", "workspace detach --stop"},
		Build: func(b Bound) (Action, error) {
			return WorkspaceAction{Sub: "detach", Stop: b.Bool("stop")}, nil
		},
	},

	// --- board ---
	//
	// CLI-only: the TUI and WebUI reach the agentboard through dedicated
	// panes, not a command line. This is the family whose `purge <topic>
	// --seq N` destroyed two live messages, which is why --seq is marked as
	// widening when unset.
	{
		Path:     []string{"board", "topics"},
		Surfaces: CLI,
		Examples: []string{"board topics"},
		Build:    func(b Bound) (Action, error) { return BoardAction{Sub: "topics"}, nil },
	},
	{
		Path:     []string{"board", "read"},
		Surfaces: CLI,
		Args:     []Arg{{Name: "topic", Type: ArgTopic}},
		Flags: []Flag{
			{Name: "in-reply-to", Type: FlagUint64, Default: uint64(0),
				Help: "only messages replying to this seq"},
			{Name: "json", Type: FlagBool, Default: false, Help: "JSON Lines instead of text"},
		},
		Examples: []string{"board read chat.abcd1234", "board read chat.abcd1234 --json"},
		Build: func(b Bound) (Action, error) {
			irt, _ := b.Flags["in-reply-to"].(uint64)
			return BoardAction{Sub: "read", Topic: b.Args[0], InReplyTo: irt, JSON: b.Bool("json")}, nil
		},
	},
	{
		Path:     []string{"board", "subscribers"},
		Surfaces: CLI,
		Args:     []Arg{{Name: "topic", Type: ArgTopic, Variadic: true}},
		Examples: []string{"board subscribers", "board subscribers chat.abcd1234"},
		Build: func(b Bound) (Action, error) {
			a := BoardAction{Sub: "subscribers"}
			if len(b.Args) > 1 {
				return nil, fmt.Errorf("board subscribers: at most one topic")
			}
			if len(b.Args) == 1 {
				a.Topic = b.Args[0]
			}
			return a, nil
		},
	},
	{
		Path:     []string{"board", "retract"},
		Surfaces: CLI,
		Args:     []Arg{{Name: "topic", Type: ArgTopic}},
		Flags: []Flag{
			{Name: "seq", Type: FlagUint64, Default: uint64(0),
				Help: "the message to withdraw; required — there is no whole-topic retract"},
		},
		Examples: []string{"board retract chat.abcd1234 --seq 42"},
		Build: func(b Bound) (Action, error) {
			seq, _ := b.Flags["seq"].(uint64)
			if seq == 0 {
				return nil, fmt.Errorf("board retract: --seq is required; there is no whole-topic retract")
			}
			return BoardAction{Sub: "retract", Topic: b.Args[0], Seq: seq}, nil
		},
	},
	{
		Path:     []string{"board", "purge"},
		Surfaces: CLI,
		Args:     []Arg{{Name: "topic", Type: ArgTopic}},
		Flags: []Flag{
			// THE flag this whole design is named after. `board purge <topic>
			// --seq N` -- the exact line the help text printed -- left --seq at
			// its zero value under stdlib parsing, which is the WHOLE-TOPIC
			// form, and destroyed two messages on a live board.
			{Name: "seq", Type: FlagUint64, Default: uint64(0), WidensIfUnset: true,
				Help: "drop one message by seq; omitted drops the whole topic ring"},
		},
		Examples: []string{"board purge chat.abcd1234", "board purge chat.abcd1234 --seq 42"},
		Build: func(b Bound) (Action, error) {
			seq, _ := b.Flags["seq"].(uint64)
			return BoardAction{Sub: "purge", Topic: b.Args[0], Seq: seq}, nil
		},
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
		Path:     []string{"submit"},
		Surfaces: CLI | TUI | WebUI,
		// The prompt is a positional on the TUI and the WebUI and --task on the
		// CLI. Both work everywhere now: the flag wins when given, the trailing
		// words are the prompt otherwise.
		Trailing: &Trailing{
			Name: "prompt", Reason: "the prompt is free-form text, so a word beginning with '-' cannot be told from a flag",
		},
		Flags:    spawnFlags(spawnSubmit),
		Examples: []string{`submit --repo /r --task "do the thing"`, `submit --repo /r do the thing`},
		Build:    buildSpawn("submit"),
	},
	{
		Path:     []string{"interactive"},
		Surfaces: CLI | TUI,
		Flags:    spawnFlags(spawnInteractive),
		Examples: []string{"interactive --repo /r"},
		Build:    buildSpawn("interactive"),
	},
	{
		Path:     []string{"session", "new"},
		Surfaces: CLI | TUI,
		Flags:    spawnFlags(spawnSessionNew),
		Examples: []string{"session new --repo /r", "session new --repo /r -d"},
		Build:    buildSpawn("session-new"),
	},
}

// Lookup finds the spec for a verb path.
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

// buildWorkspaceOptionalName is the Build for the workspace verbs whose name
// may be omitted, meaning the installed workspace.
func buildWorkspaceOptionalName(sub string) func(Bound) (Action, error) {
	return func(b Bound) (Action, error) {
		a := WorkspaceAction{Sub: sub}
		if len(b.Args) > 1 {
			return nil, fmt.Errorf("workspace %s: at most one name", sub)
		}
		if len(b.Args) == 1 {
			a.Name = b.Args[0]
		}
		return a, nil
	}
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
