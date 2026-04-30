package runner

// mergeExtraArgs concatenates the runner-global --claude-args baseline with
// per-task extras supplied by the originating client (cli / tui / webui).
// The global baseline comes first so that per-task flags appear later on
// the claude command line — claude's flag parser is largely last-wins,
// which gives the per-task value precedence on conflict (e.g. global
// `--allowedTools foo` followed by per-task `--allowedTools bar` runs
// claude with bar). A fresh slice is returned so the caller can mutate
// it without touching the runner's stored config.
func mergeExtraArgs(global, perTask []string) []string {
	if len(global) == 0 && len(perTask) == 0 {
		return nil
	}
	out := make([]string, 0, len(global)+len(perTask))
	out = append(out, global...)
	out = append(out, perTask...)
	return out
}
