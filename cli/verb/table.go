package verb

import (
	"fmt"
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
