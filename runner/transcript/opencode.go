package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	cmd := exec.CommandContext(ctx, opencodeBin, "session", "list", "--format", "json")
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
		if r.ID != "" && SamePath(r.Directory, dir) {
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
	cmd := exec.Command(opencodeBin, "export", id)
	cmd.Dir = nearestExisting(dir)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode export: %v", err)
	}
	return &cmdReader{cmd: cmd, r: stdout, stderr: stderr}, nil
}

// cmdReader streams a command's stdout and reports the command's failure at
// the end of the read and at Close, so a failed export is never a short file
// passed off as complete.
type cmdReader struct {
	cmd    *exec.Cmd
	r      io.Reader
	stderr *bytes.Buffer
	done   bool
	err    error
}

func (c *cmdReader) wait() error {
	if !c.done {
		c.done = true
		if err := c.cmd.Wait(); err != nil {
			c.err = fmt.Errorf("opencode export: %v: %s", err, strings.TrimSpace(c.stderr.String()))
		}
	}
	return c.err
}

func (c *cmdReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		if werr := c.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (c *cmdReader) Close() error {
	if !c.done && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.wait()
}
