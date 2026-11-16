package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/runner/hostcmd"
	"path/filepath"
	"strings"
	"time"
)

// opencodeBin is the binary run for opencode's own listing and export; a
// variable so a test can stand in a script.
var opencodeBin = "opencode"

// opencodeListTimeout bounds `opencode session list`. Listing answers a
// person waiting on `transcript ls`, so a wedged opencode must not hold it.
const opencodeListTimeout = 60 * time.Second

// opencodeStore reads opencode through its own CLI (B-D4, B-F9): its store is
// one database holding every session of every project, so returning a file
// would hand out other projects' conversations. `session list --format json`
// lists the project's sessions with the directory each ran in; `export <id>`
// writes one session as JSON.
type opencodeStore struct{ getenv func(string) string }

func newOpencode(getenv func(string) string) (Store, error) {
	return &opencodeStore{getenv: getenv}, nil
}

func (s *opencodeStore) Agent() string { return "opencode" }

// nearestExisting returns dir, or its nearest ancestor that exists: a finished
// task's worktree may be gone, and opencode answers for the project the
// directory belongs to, which an ancestor inside the repo still is.
func nearestExisting(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		if filepath.Dir(d) == d {
			return d
		}
	}
}

type opencodeRow struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Created   int64  `json:"created"`
	Updated   int64  `json:"updated"`
}

func (s *opencodeStore) list(dir string) ([]opencodeRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opencodeListTimeout)
	defer cancel()
	cmd := hostcmd.CommandContext(ctx, opencodeBin, "session", "list", "--format", "json")
	cmd.Dir = nearestExisting(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode session list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	// Outside any project opencode prints nothing: no sessions, not an error.
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var rows []opencodeRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("opencode session list: %v", err)
	}
	var mine []opencodeRow
	for _, r := range rows {
		if localID(r.ID) && !strings.HasPrefix(r.ID, "-") && SamePath(r.Directory, dir) {
			mine = append(mine, r)
		}
	}
	return mine, nil
}

func (s *opencodeStore) Sessions(dir string) ([]Session, error) {
	rows, err := s.list(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		// Size is unknown until exported; 0 says so beside a file whose
		// size_known is false.
		out = append(out, Session{Agent: "opencode", ID: r.ID, Created: time.UnixMilli(r.Created), Updated: time.UnixMilli(r.Updated)})
	}
	return out, nil
}

const opencodeExportName = "export.json"

func (s *opencodeStore) find(dir, id string) error {
	rows, err := s.list(dir)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == id {
			return nil
		}
	}
	return ErrNoSession
}

func (s *opencodeStore) Files(dir, id string) ([]File, error) {
	if err := s.find(dir, id); err != nil {
		return nil, err
	}
	return []File{{Name: opencodeExportName}}, nil
}

func (s *opencodeStore) Open(dir, id, name string) (io.ReadCloser, error) {
	if err := s.find(dir, id); err != nil {
		return nil, err
	}
	if name != opencodeExportName {
		return nil, ErrNoFile
	}
	f, err := NewSpool()
	if err != nil {
		return nil, err
	}
	// The export goes to a file, never a pipe: opencode 1.18.18 ends a piped
	// export at 128KiB and still exits 0 (measured 2026-11-16: 131072 of
	// 479799 bytes, every run), while a file receives all of it.
	cmd := hostcmd.Command(opencodeBin, "export", id)
	cmd.Dir = nearestExisting(dir)
	cmd.Stdout = f.File
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		f.Close()
		return nil, fmt.Errorf("opencode export: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	// The exit status did not catch the cut, so the output is checked too: a
	// short file is never passed off as complete.
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		err = oneJSONValue(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("opencode export: the output is not one complete JSON value: %v", err)
		}
	} else {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// oneJSONValue reads r to its end and reports whether it held exactly one
// complete JSON value, token by token, so a large export is never held whole.
func oneJSONValue(r io.Reader) error {
	dec := json.NewDecoder(r)
	depth, seen := 0, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		seen = true
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			if _, err := dec.Token(); err != io.EOF {
				return errors.New("more data after the value")
			}
			return nil
		}
	}
	if !seen {
		return errors.New("empty")
	}
	return errors.New("it ends inside the value")
}
