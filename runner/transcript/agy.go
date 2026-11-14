package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/hostcmd"
)

// sqlite3Bin reads agy's index (operator, 2026-11-15: the sqlite3 CLI rather
// than a Go SQLite module); a variable so a test can take it away.
var sqlite3Bin = "sqlite3"

// agyIndexTimeout bounds one read of the index, as opencodeListTimeout does
// for opencode's listing.
const agyIndexTimeout = 30 * time.Second

// agyStore reads agy's store, ~/.gemini/antigravity-cli (B-F8): one SQLite
// file per conversation under conversations/, its brain/<id>/ beside them,
// and the conversation→workspace index conversation_summaries.db (B-F10).
//
// What this knows about agy's layout — those names, the table and its
// columns — is agy's private format. When agy changes it the runner needs a
// matching change, so a store that exists but no longer reads as expected is
// an error naming what is missing, never an empty list.
type agyStore struct{ root string }

func newAgy(getenv func(string) string) (Store, error) {
	home := getenv("HOME")
	if runtime.GOOS == "windows" {
		home = getenv("USERPROFILE")
	}
	if home == "" {
		return nil, errors.New("no home directory in the agent's environment")
	}
	return &agyStore{root: filepath.Join(home, ".gemini", "antigravity-cli")}, nil
}

func (s *agyStore) Agent() string { return "agy" }

type agyRow struct {
	ID        string `json:"conversation_id"`
	Workspace string `json:"workspace_uris"`
	Modified  string `json:"last_modified_time"`
	updated   time.Time
}

// rows reads the conversations whose workspace is dir. The index is read
// whole and matched here, so no caller's path reaches the SQL.
func (s *agyStore) rows(dir string) ([]agyRow, error) {
	if _, err := os.Stat(s.root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil // agy never ran on this host
	}
	index := filepath.Join(s.root, "conversation_summaries.db")
	if _, err := os.Stat(index); err != nil {
		return nil, fmt.Errorf("%s: %v (agy's layout differs from what this runner reads)", index, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agyIndexTimeout)
	defer cancel()
	cmd := hostcmd.CommandContext(ctx, sqlite3Bin, "-readonly", "-json", index,
		"SELECT conversation_id, workspace_uris, last_modified_time FROM conversation_summaries")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sqlite3 %s: %v: %s", index, err, strings.TrimSpace(stderr.String()))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil // an empty index prints nothing
	}
	var all []agyRow
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("sqlite3 %s: %v", index, err)
	}
	var mine []agyRow
	for _, r := range all {
		if !localID(r.ID) || !agyWorkspaceIs(r.Workspace, dir) {
			continue
		}
		r.updated, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", r.Modified)
		mine = append(mine, r)
	}
	return mine, nil
}

// agyWorkspaceIs reports whether a workspace_uris value — a JSON array of
// file URIs, empty for a conversation with no workspace — names dir. The
// first URI is the conversation's own workspace; later ones are directories
// added to it (agy --add-dir), which do not make it that directory's session.
func agyWorkspaceIs(uris, dir string) bool {
	var list []string
	if uris == "" || json.Unmarshal([]byte(uris), &list) != nil || len(list) == 0 {
		return false
	}
	u, err := url.Parse(list[0])
	if err != nil || u.Scheme != "file" {
		return false
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' { // file:///C:/x on Windows
		p = p[1:]
	}
	return SamePath(filepath.FromSlash(p), dir)
}

func (s *agyStore) find(dir, id string) (agyRow, error) {
	rows, err := s.rows(dir)
	if err != nil {
		return agyRow{}, err
	}
	for _, r := range rows {
		if r.ID == id {
			return r, nil
		}
	}
	return agyRow{}, ErrNoSession
}

// files lists a conversation's files relative to the store's root: its .db
// and the -wal / -shm beside it, and everything under brain/<id>/ (B-F1a).
func (s *agyStore) files(id string) []File {
	var rel []string
	for _, suffix := range []string{".db", ".db-wal", ".db-shm"} {
		rel = append(rel, "conversations/"+id+suffix)
	}
	rel = append(rel, walkRegular(s.root, "brain/"+id)...)
	var out []File
	for _, r := range rel {
		fi, ok := lstatRegular(s.root, r)
		if !ok {
			continue
		}
		out = append(out, File{Name: r, Size: fi.Size(), SizeKnown: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *agyStore) Sessions(dir string) ([]Session, error) {
	rows, err := s.rows(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		// The index keeps no creation time; Created stays zero (shown "-").
		row := Session{Agent: "agy", ID: r.ID, Updated: r.updated, SizeKnown: true}
		var newest time.Time
		for _, f := range s.files(r.ID) {
			row.Size += f.Size
			if fi, ok := lstatRegular(s.root, f.Name); ok && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		// agy writes a zero time for some conversations; its files know better.
		if row.Updated.Year() <= 1 {
			row.Updated = newest
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.Before(out[j].Updated) })
	return out, nil
}

func (s *agyStore) Files(dir, id string) ([]File, error) {
	if _, err := s.find(dir, id); err != nil {
		return nil, err
	}
	return s.files(id), nil
}

func (s *agyStore) Open(dir, id, name string) (io.ReadCloser, error) {
	if _, err := s.find(dir, id); err != nil {
		return nil, err
	}
	for _, f := range s.files(id) {
		if f.Name == name {
			fh, _, err := openRegular(s.root, name)
			if err != nil {
				return nil, ErrNoFile
			}
			return fh, nil
		}
	}
	return nil, ErrNoFile
}
