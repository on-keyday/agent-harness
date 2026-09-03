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
