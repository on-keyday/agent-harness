package streamagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// Transcript replay: a resumed session shows the operator the conversation the
// agent resumed. claude's stream-json output carries only what happens after
// the resume, while `--continue` restores the agent's context from the session
// transcript on disk, so without this the agent remembers a conversation the
// operator cannot see. Design:
// docs/superpowers/specs/2026-09-30-stream-resume-transcript-replay-design.md §3.

// Replay bounds (§3.4). The server's per-task ring holds about 1 MiB and
// evicts from the front, and both chat views keep 400 lines, so a replay is
// kept well inside both. It keeps the END of the conversation: the recent
// turns are what the operator needs to write the next one.
const (
	replayBudgetBytes = 256 << 10
	replayMaxEvents   = 300
	replayFieldCap    = 2 << 10  // Args, Result
	replayTextCap     = 16 << 10 // Text
)

// transcriptReplay is what a replay found.
type transcriptReplay struct {
	Path      string
	SessionID string  // the transcript's file name without .jsonl
	Events    []Event // already Replay-marked and bounded
	Total     int     // conversation events before the bound was applied
}

// claudeConfigDir is where claude keeps its state: $CLAUDE_CONFIG_DIR, else
// ~/.claude. getenv is the agent's environment, which is the adapter's own.
func claudeConfigDir(getenv func(string) string) (string, error) {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// encodeProjectDirName is claude's name for a working directory under
// projects/: every character that is not an ASCII letter or digit becomes '-'.
// Documented at code.claude.com/docs/en/agent-sdk/sessions.
func encodeProjectDirName(absDir string) string {
	var b strings.Builder
	for i := 0; i < len(absDir); i++ {
		c := absDir[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// projectDirNameLimit is the length past which claude truncates a project
// directory name and appends a hash. The hash is not documented, so a longer
// name is matched on this prefix instead.
const projectDirNameLimit = 200

// findClaudeTranscript returns the transcript `--continue` resumes in dir: the
// newest *.jsonl under the project directory claude uses for it. That is the
// documented `--continue` rule ("the most recent session in the current
// directory"), read the same way here rather than asked of the agent.
func findClaudeTranscript(getenv func(string) string, dir string) (string, error) {
	cfg, err := claudeConfigDir(getenv)
	if err != nil {
		return "", fmt.Errorf("no claude config dir: %w", err)
	}
	projects := filepath.Join(cfg, "projects")

	var dirs []string
	if name := getenv("CLAUDE_CODE_PROJECT_DIR_NAME"); name != "" {
		dirs = []string{filepath.Join(projects, name)}
	} else {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		name := encodeProjectDirName(abs)
		if len(name) <= projectDirNameLimit {
			dirs = []string{filepath.Join(projects, name)}
		} else {
			entries, err := os.ReadDir(projects)
			if err != nil {
				return "", err
			}
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), name[:projectDirNameLimit]) {
					dirs = append(dirs, filepath.Join(projects, e.Name()))
				}
			}
		}
	}

	var best string
	var bestMod int64
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue // a missing directory is "no transcript", reported below
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if m := info.ModTime().UnixNano(); best == "" || m > bestMod {
				best, bestMod = filepath.Join(d, e.Name()), m
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("no transcript under %s for %s", projects, dir)
	}
	return best, nil
}

// transcriptRow is the part of a transcript line that decides whether it is
// replayed. The line itself goes to the decoder unchanged: a conversation row
// is a stream-json envelope plus fields the decoder ignores.
type transcriptRow struct {
	Type        string  `json:"type"`
	UUID        string  `json:"uuid"`
	ParentUUID  *string `json:"parentUuid"`
	IsMeta      bool    `json:"isMeta"`
	IsSidechain bool    `json:"isSidechain"`
}

// readClaudeTranscript selects the conversation in a transcript and converts
// it to bounded, Replay-marked neutral events (§3.2–3.4).
func readClaudeTranscript(path string) (*transcriptReplay, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type row struct {
		transcriptRow
		line []byte
	}
	var rows []row
	parent := map[string]string{} // uuid -> parentUuid, over EVERY row with a uuid
	rd := bufio.NewReader(f)
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(trimSpace(line)) > 0 {
			var r transcriptRow
			// A line that does not parse is skipped: one bad line must not cost
			// the operator the rest of the history.
			if json.Unmarshal(line, &r) == nil {
				if r.UUID != "" {
					p := ""
					if r.ParentUUID != nil {
						p = *r.ParentUUID
					}
					parent[r.UUID] = p
				}
				rows = append(rows, row{r, line})
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, rerr
		}
	}

	isConv := func(r transcriptRow) bool {
		return (r.Type == "user" || r.Type == "assistant") && !r.IsSidechain && r.UUID != ""
	}
	// The leaf is the last conversation row. Walking parentUuid from it keeps
	// an abandoned branch (a rewind) out, and the walk ends where a parent is
	// missing, which is where a compaction boundary cuts the chain.
	leaf := ""
	for i := len(rows) - 1; i >= 0; i-- {
		if isConv(rows[i].transcriptRow) {
			leaf = rows[i].UUID
			break
		}
	}
	onChain := map[string]bool{}
	for u := leaf; u != "" && !onChain[u]; {
		p, ok := parent[u]
		if !ok {
			break
		}
		onChain[u] = true
		u = p
	}

	dec := agentlog.NewDecoder("claude-stream-json")
	var events []Event
	for _, r := range rows {
		if !isConv(r.transcriptRow) || r.IsMeta || !onChain[r.UUID] {
			continue
		}
		for _, e := range dec.Decode(r.line) {
			if e.Kind == agentlog.KindRaw {
				continue // not conversation; never show a transcript's JSON
			}
			ev := FromAgentlog(e)
			ev.Replay = true
			ev.Text = agentlog.TruncateBytes(ev.Text, replayTextCap)
			ev.Args = agentlog.TruncateBytes(ev.Args, replayFieldCap)
			ev.Result = agentlog.TruncateBytes(ev.Result, replayFieldCap)
			events = append(events, ev)
		}
	}

	out := &transcriptReplay{
		Path:      path,
		SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Total:     len(events),
	}
	// Keep from the end, until the byte budget or the event cap is reached.
	size, start := 0, len(events)
	for start > 0 && len(events)-start < replayMaxEvents {
		b, _ := json.Marshal(events[start-1])
		if size+len(b) > replayBudgetBytes {
			break
		}
		size += len(b)
		start--
	}
	out.Events = events[start:]
	return out, nil
}

// replayTranscript writes the replay for a resume in dir, before the agent
// speaks (§3.5). A failure is one warning, never fatal (§3.7): a missing
// replay must not cost the operator the session. It returns the replayed
// session id, or "" when nothing was replayed.
func (a *claudeAdapter) replayTranscript(getenv func(string) string, dir string) string {
	warn := func(format string, args ...any) {
		_ = a.w.Event(Event{Kind: EventError, Warning: true, Replay: true,
			Text: "no earlier conversation to show: " + fmt.Sprintf(format, args...)})
	}
	path, err := findClaudeTranscript(getenv, dir)
	if err != nil {
		warn("%v", err)
		return ""
	}
	tr, err := readClaudeTranscript(path)
	if err != nil {
		warn("%v", err)
		return ""
	}
	if len(tr.Events) == 0 {
		warn("the transcript %s holds no conversation", filepath.Base(path))
		return ""
	}
	_ = a.w.Event(Event{Kind: EventRaw, Replay: true,
		Text: fmt.Sprintf("── previous conversation: last %d of %d events ──", len(tr.Events), tr.Total)})
	for _, ev := range tr.Events {
		_ = a.w.Event(ev)
	}
	_ = a.w.Event(Event{Kind: EventRaw, Replay: true, Text: "── resumed ──"})
	return tr.SessionID
}
