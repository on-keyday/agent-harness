package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// New returns an empty file, for a first `workspace save`.
func New() *File { return &File{} }

// Set replaces the named workspace's lines with a rendering of ws, leaving
// every other line — including comments and whatever spacing the operator
// chose — byte-identical. An unknown name is appended at the end.
//
// Replacing a line SPAN rather than rewriting the whole file is why this format
// is line-oriented: a structured encoder would have to reproduce the operator's
// own text, and would not.
func (f *File) Set(ws *Workspace) {
	block := renderWorkspace(ws)
	old, exists := f.Workspace(ws.Name)
	if !exists {
		if len(f.lines) > 0 && strings.TrimSpace(f.lines[len(f.lines)-1]) != "" {
			f.lines = append(f.lines, "")
		}
		start := len(f.lines)
		f.lines = append(f.lines, block...)
		clone := *ws
		clone.start, clone.end = start, len(f.lines)
		f.ws = append(f.ws, &clone)
		return
	}
	// Rebuild the slice rather than appending into f.lines[:old.start]: that
	// would alias the tail it is about to overwrite.
	delta := len(block) - (old.end - old.start)
	next := make([]string, 0, len(f.lines)+delta)
	next = append(next, f.lines[:old.start]...)
	next = append(next, block...)
	next = append(next, f.lines[old.end:]...)
	f.lines = next

	for _, w := range f.ws {
		if w.start > old.start {
			w.start += delta
			w.end += delta
		}
	}
	clone := *ws
	clone.start, clone.end = old.start, old.start+len(block)
	*old = clone
}

// renderWorkspace emits the block for one workspace: its own keys, then one
// task block per task. A key whose value is empty is omitted — a bare `repo =`
// line would parse back as an explicit empty repo, which is not the same as
// never having written one. `grid` is the exception: its empty value is a real
// selection (the unnarrowed grid), so GridSet decides it instead.
func renderWorkspace(ws *Workspace) []string {
	out := []string{fmt.Sprintf("[workspace %s]", ws.Name)}
	for _, kv := range [][2]string{
		{"server-cid", ws.ServerCID},
		{"ws-path", ws.WSPath},
		{"repo", ws.Repo},
	} {
		if kv[1] != "" {
			out = append(out, fmt.Sprintf("%-10s = %s", kv[0], kv[1]))
		}
	}
	if ws.GridSet {
		out = append(out, strings.TrimRight(fmt.Sprintf("%-10s = %s", "grid", ws.Grid), " "))
	}
	for _, t := range ws.Tasks {
		resume := t.Resume
		if resume == "" {
			resume = ResumeNo
		}
		runner := t.Runner
		if runner == "" {
			runner = RunnerAssigned
		}
		out = append(out, "",
			fmt.Sprintf("[workspace %s task %s]", ws.Name, t.ID),
			fmt.Sprintf("%-8s = %s", "resume", resume),
			fmt.Sprintf("%-8s = %s", "runner", runner))
		for _, fw := range t.Forwards {
			out = append(out, fmt.Sprintf("%-8s = %s", "forward", fw))
		}
	}
	return out
}

// Block renders one workspace exactly as Set would write it, so `workspace
// show` and the file cannot disagree.
func Block(ws *Workspace) []byte {
	return []byte(strings.Join(renderWorkspace(ws), "\n") + "\n")
}

// Save writes the file, creating the parent directory. 0o600: a workspace
// carries a server address and repository paths rather than secrets, but it is
// the operator's own file and there is no reason for it to be group-readable.
func (f *File) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, f.Render(), 0o600)
}
