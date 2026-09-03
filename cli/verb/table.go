package verb

import "time"

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
