// Package workspace reads and writes .harness/config: named client-side
// workspaces describing which task to bring back, which forwards to establish
// and which grid to open when a client starts, reconnects, or is asked to
// re-apply.
//
// This package owns the FILE, not the value grammars inside it. A forward value
// is handed to cli.ParseForwardSpec / cli.ParseRemoteForwardSpec and a grid
// value to cli.ParseGridArgs, so the config can never accept a spelling the
// command line rejects.
package workspace

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Resume says what an apply does with a task found in a terminal state.
type Resume string

const (
	ResumeNo       Resume = "no"       // leave it alone (default)
	ResumeContinue Resume = "continue" // resume keeping the agent's conversation (r/u)
	ResumeFresh    Resume = "fresh"    // resume without it (R/U)
)

// Runner says which runner may take a resumed task.
type Runner string

const (
	RunnerAssigned Runner = "assigned" // the one it last ran on (r/R); default
	RunnerAny      Runner = "any"      // any runner (u/U)
)

// Task is one [workspace <name> task <id>] block.
type Task struct {
	ID       string // 32 lowercase hex
	Resume   Resume
	Runner   Runner
	Forwards []string // verbatim "-L …" / "-R …", in file order
}

// Workspace is one [workspace <name>] block plus its task blocks.
type Workspace struct {
	Name      string
	ServerCID string
	WSPath    string
	Repo      string
	// Grid is the `grid` command's argument string, verbatim. GridSet
	// separates an absent key (do not touch the grid) from `grid =` with an
	// empty value (open the unnarrowed grid) — the empty string is a
	// meaningful value here, so presence needs its own bit.
	Grid    string
	GridSet bool
	Tasks   []Task

	// start, end is the half-open span of File.lines this workspace occupies.
	// end advances only when a line is ASSIGNED to this workspace — a header or
	// a key — so trailing blank lines and the comments that introduce the NEXT
	// workspace stay outside it. Ending the span at the next header instead
	// would make Set delete that preamble.
	start, end int
}

// File is a parsed .harness/config with every source line retained.
type File struct {
	lines []string
	ws    []*Workspace
}

// Parse reads a config. Every unrecognised header, key or enum value is an
// error: a typo that silently established nothing is the failure this rejects.
func Parse(r io.Reader) (*File, error) {
	f := &File{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur *Workspace
	var curTask *Task // points into cur.Tasks
	lineNo := 0
	for sc.Scan() {
		raw := sc.Text()
		f.lines = append(f.lines, raw)
		lineNo++
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: unterminated section header %q", lineNo, line)
			}
			name, taskID, err := parseHeader(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"), lineNo)
			if err != nil {
				return nil, err
			}
			if taskID == "" {
				if _, dup := f.Workspace(name); dup {
					return nil, fmt.Errorf("line %d: workspace %q declared twice", lineNo, name)
				}
				cur = &Workspace{Name: name, start: lineNo - 1, end: lineNo}
				f.ws = append(f.ws, cur)
				curTask = nil
				continue
			}
			if cur == nil || cur.Name != name {
				return nil, fmt.Errorf("line %d: task block names workspace %q, which is not the open one", lineNo, name)
			}
			cur.Tasks = append(cur.Tasks, Task{ID: taskID, Resume: ResumeNo, Runner: RunnerAssigned})
			curTask = &cur.Tasks[len(cur.Tasks)-1]
			cur.end = lineNo
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: %q appears before any [workspace …] header", lineNo, line)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: %q is not `key = value`", lineNo, line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if err := assign(cur, curTask, key, val, lineNo); err != nil {
			return nil, err
		}
		cur.end = lineNo
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return f, nil
}

// parseHeader accepts "workspace <name>" and "workspace <name> task <32hex>".
func parseHeader(body string, lineNo int) (name, taskID string, err error) {
	tok := strings.Fields(body)
	switch {
	case len(tok) == 2 && tok[0] == "workspace":
		return tok[1], "", nil
	case len(tok) == 4 && tok[0] == "workspace" && tok[2] == "task":
		id := strings.ToLower(tok[3])
		if !isHex32(id) {
			return "", "", fmt.Errorf("line %d: %q is not a 32-hex task id", lineNo, tok[3])
		}
		return tok[1], id, nil
	}
	return "", "", fmt.Errorf("line %d: unknown section header [%s] (want [workspace <name>] or [workspace <name> task <32-hex>])", lineNo, body)
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// assign writes one key into the workspace or the open task block. Which keys
// are legal depends on which block is open, so an operator who puts `forward`
// under [workspace w] is told where it belongs rather than silently ignored.
func assign(ws *Workspace, tk *Task, key, val string, lineNo int) error {
	if tk != nil {
		switch key {
		case "resume":
			switch Resume(val) {
			case ResumeNo, ResumeContinue, ResumeFresh:
				tk.Resume = Resume(val)
				return nil
			}
			return fmt.Errorf("line %d: resume = %q (want no, continue or fresh)", lineNo, val)
		case "runner":
			switch Runner(val) {
			case RunnerAssigned, RunnerAny:
				tk.Runner = Runner(val)
				return nil
			}
			return fmt.Errorf("line %d: runner = %q (want assigned or any)", lineNo, val)
		case "forward":
			tk.Forwards = append(tk.Forwards, val)
			return nil
		}
		return fmt.Errorf("line %d: unknown key %q in a task block (want resume, runner or forward)", lineNo, key)
	}
	switch key {
	case "server-cid":
		ws.ServerCID = val
	case "ws-path":
		ws.WSPath = val
	case "repo":
		ws.Repo = val
	case "grid":
		ws.Grid, ws.GridSet = val, true
	default:
		return fmt.Errorf("line %d: unknown key %q in a workspace block (want server-cid, ws-path, repo or grid)", lineNo, key)
	}
	return nil
}

// Workspace returns the named workspace. A nil receiver reports absence rather
// than panicking, so a client with no config file needs no guard at each use.
func (f *File) Workspace(name string) (*Workspace, bool) {
	if f == nil {
		return nil, false
	}
	for _, w := range f.ws {
		if w.Name == name {
			return w, true
		}
	}
	return nil, false
}

// Names lists the workspaces in file order.
func (f *File) Names() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.ws))
	for _, w := range f.ws {
		out = append(out, w.Name)
	}
	return out
}

// Render returns the file's bytes. Lines this package never modified come back
// exactly as they were read, comments included.
//
// Parse drops each line's trailing newline and Render adds exactly one back, so
// a source file that did not end in a newline gains one.
func (f *File) Render() []byte {
	if f == nil || len(f.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(f.lines, "\n") + "\n")
}
