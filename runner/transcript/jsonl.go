package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lineStore is a store whose sessions are JSON-lines files that record their
// working directory in an early line: claude, codex and pi (B-F1).
type lineStore struct {
	agent string
	// candidates returns the main files that may belong to dir.
	candidates func(dir string) ([]string, error)
	// header reads a main file's recorded cwd, session id and start time.
	header func(path string) (h header, ok bool)
	// companions returns the files beside main that belong to the session,
	// relative to base, and base itself (the directory names are relative to).
	companions func(main, id string) (base string, rel []string)
}

type header struct {
	cwd, id string
	created time.Time
}

func (s *lineStore) Agent() string { return s.agent }

// match is one main file recorded in the directory asked about.
type match struct {
	path string
	h    header
}

func (s *lineStore) matches(dir string) ([]match, error) {
	paths, err := s.candidates(dir)
	if err != nil {
		return nil, err
	}
	var out []match
	for _, p := range paths {
		h, ok := s.header(p)
		if ok && SamePath(h.cwd, dir) {
			out = append(out, match{p, h})
		}
	}
	return out, nil
}

func (s *lineStore) find(dir, id string) (match, error) {
	ms, err := s.matches(dir)
	if err != nil {
		return match{}, err
	}
	for _, m := range ms {
		if m.h.id == id {
			return m, nil
		}
	}
	return match{}, ErrNoSession
}

// files lists m's files: the main file and its companions, relative to base.
func (s *lineStore) files(m match) (string, []File) {
	base, rel := s.companions(m.path, m.h.id)
	var out []File
	for _, r := range rel {
		fi, err := os.Stat(filepath.Join(base, filepath.FromSlash(r)))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, File{Name: r, Size: fi.Size(), SizeKnown: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return base, out
}

func (s *lineStore) Sessions(dir string) ([]Session, error) {
	ms, err := s.matches(dir)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, m := range ms {
		base, fs := s.files(m)
		row := Session{Agent: s.agent, ID: m.h.id, Created: m.h.created}
		for _, f := range fs {
			row.Size += f.Size
			if fi, err := os.Stat(filepath.Join(base, filepath.FromSlash(f.Name))); err == nil && fi.ModTime().After(row.Updated) {
				row.Updated = fi.ModTime()
			}
		}
		if row.Created.IsZero() {
			row.Created = row.Updated
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func (s *lineStore) Files(dir, id string) ([]File, error) {
	m, err := s.find(dir, id)
	if err != nil {
		return nil, err
	}
	_, fs := s.files(m)
	return fs, nil
}

func (s *lineStore) Open(dir, id, name string) (io.ReadCloser, error) {
	m, err := s.find(dir, id)
	if err != nil {
		return nil, err
	}
	base, fs := s.files(m)
	for _, f := range fs {
		if f.Name == name {
			return os.Open(filepath.Join(base, filepath.FromSlash(name)))
		}
	}
	return nil, ErrNoFile
}

// headerLines reads at most max lines of path as JSON objects and hands each
// to visit until it returns true. A line longer than 1 MiB is skipped
// unread: a header is short, and a tool output can be very long.
func headerLines(path string, max int, visit func(map[string]json.RawMessage) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	const lineMax = 1 << 20
	for n := 0; n < max; n++ {
		var line []byte
		skip := false
		for {
			chunk, isPrefix, err := r.ReadLine()
			if err != nil {
				return
			}
			if !skip {
				line = append(line, chunk...)
				if len(line) > lineMax {
					skip, line = true, nil
				}
			}
			if !isPrefix {
				break
			}
		}
		if skip {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(line, &obj) == nil && visit(obj) {
			return
		}
	}
}

func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func jsonTime(raw json.RawMessage) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, jsonString(raw))
	return t
}

// --- claude -----------------------------------------------------------------

// ClaudeProjectDirNameLimit is the length past which claude truncates a
// project directory name and appends an undocumented hash (B-F2).
const ClaudeProjectDirNameLimit = 200

// ClaudeProjectDirName is claude's name for a working directory under
// projects/: every character that is not an ASCII letter or digit becomes '-'.
// Documented at code.claude.com/docs/en/agent-sdk/sessions.
func ClaudeProjectDirName(absDir string) string {
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

// ClaudeConfigDir is where claude keeps its state: $CLAUDE_CONFIG_DIR, else
// ~/.claude.
func ClaudeConfigDir(getenv func(string) string) (string, error) {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func newClaude(getenv func(string) string) (Store, error) {
	cfg, err := ClaudeConfigDir(getenv)
	if err != nil {
		return nil, err
	}
	projects := filepath.Join(cfg, "projects")
	return &lineStore{
		agent: "claude",
		candidates: func(dir string) ([]string, error) {
			name := ClaudeProjectDirName(dir)
			var dirs []string
			if len(name) <= ClaudeProjectDirNameLimit {
				dirs = []string{filepath.Join(projects, name)}
			} else {
				entries, err := os.ReadDir(projects)
				if err != nil && !os.IsNotExist(err) {
					return nil, err
				}
				for _, e := range entries {
					if e.IsDir() && strings.HasPrefix(e.Name(), name[:ClaudeProjectDirNameLimit]) {
						dirs = append(dirs, filepath.Join(projects, e.Name()))
					}
				}
			}
			var out []string
			for _, d := range dirs {
				ms, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
				out = append(out, ms...)
			}
			return out, nil
		},
		header: func(path string) (header, bool) {
			h := header{id: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
			found := false
			headerLines(path, 64, func(o map[string]json.RawMessage) bool {
				if c, ok := o["cwd"]; ok {
					h.cwd, h.created, found = jsonString(c), jsonTime(o["timestamp"]), true
					return true
				}
				return false
			})
			return h, found
		},
		companions: func(main, id string) (string, []string) {
			base := filepath.Dir(main)
			rel := []string{filepath.Base(main)}
			_ = filepath.WalkDir(filepath.Join(base, id), func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					if r, rerr := filepath.Rel(base, p); rerr == nil {
						rel = append(rel, filepath.ToSlash(r))
					}
				}
				return nil
			})
			return base, rel
		},
	}, nil
}

// --- codex ------------------------------------------------------------------

func newCodex(getenv func(string) string) (Store, error) {
	home := getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(h, ".codex")
	}
	sessions := filepath.Join(home, "sessions")
	return &lineStore{
		agent: "codex",
		candidates: func(string) ([]string, error) {
			var out []string
			err := filepath.WalkDir(sessions, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if os.IsNotExist(err) {
						return filepath.SkipAll
					}
					return nil
				}
				if d.Type().IsRegular() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
					out = append(out, p)
				}
				return nil
			})
			return out, err
		},
		header: func(path string) (header, bool) {
			var h header
			found := false
			headerLines(path, 1, func(o map[string]json.RawMessage) bool {
				if jsonString(o["type"]) != "session_meta" {
					return true
				}
				var p struct {
					ID  string `json:"id"`
					Cwd string `json:"cwd"`
				}
				if json.Unmarshal(o["payload"], &p) == nil && p.Cwd != "" {
					h = header{cwd: p.Cwd, id: p.ID, created: jsonTime(o["timestamp"])}
					found = h.id != ""
				}
				return true
			})
			return h, found
		},
		companions: func(main, _ string) (string, []string) {
			return filepath.Dir(main), []string{filepath.Base(main)}
		},
	}, nil
}

// --- pi ---------------------------------------------------------------------

func newPi(getenv func(string) string) (Store, error) {
	dir := getenv("PI_CODING_AGENT_SESSION_DIR")
	if dir == "" {
		agent := getenv("PI_CODING_AGENT_DIR")
		if agent == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			agent = filepath.Join(h, ".pi", "agent")
		}
		dir = filepath.Join(agent, "sessions")
	}
	return &lineStore{
		agent: "pi",
		candidates: func(string) ([]string, error) {
			ms, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
			return ms, err
		},
		header: func(path string) (header, bool) {
			var h header
			found := false
			headerLines(path, 1, func(o map[string]json.RawMessage) bool {
				if jsonString(o["type"]) == "session" {
					h = header{cwd: jsonString(o["cwd"]), id: jsonString(o["id"]), created: jsonTime(o["timestamp"])}
					found = h.cwd != "" && h.id != ""
				}
				return true
			})
			return h, found
		},
		companions: func(main, _ string) (string, []string) {
			return filepath.Dir(main), []string{filepath.Base(main)}
		},
	}, nil
}
