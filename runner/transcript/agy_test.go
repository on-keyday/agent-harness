package transcript

import (
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// agy's index as agy 1.5.8 creates it (B-F1, B-F10); the fixture uses the
// real sqlite3, so a query that does not fit agy's schema fails here.
const agySchema = "CREATE TABLE `conversation_summaries` (`conversation_id` text,`title` text NOT NULL DEFAULT \"\",`preview` text NOT NULL DEFAULT \"\",`step_count` integer NOT NULL DEFAULT 0,`last_modified_time` datetime NOT NULL,`workspace_uris` text NOT NULL,`status` text NOT NULL DEFAULT \"\",`source` text NOT NULL DEFAULT \"\",`project_id` text NOT NULL DEFAULT \"\",`agent_name` text NOT NULL DEFAULT \"\",`parent_conversation_id` text NOT NULL DEFAULT \"\",`nesting_depth` integer NOT NULL DEFAULT 0,`battle_id` text NOT NULL DEFAULT \"\",`winning_conversation_id` text NOT NULL DEFAULT \"\",`not_fully_idle` numeric NOT NULL DEFAULT false,`killed` numeric NOT NULL DEFAULT false,`last_user_input_time` datetime NOT NULL,`last_user_input_step_index` integer NOT NULL DEFAULT -1,`app_data_dir` text NOT NULL DEFAULT \"\", raw_summary BLOB, group_id TEXT NOT NULL DEFAULT '',PRIMARY KEY (`conversation_id`));"

func needSqlite3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH")
	}
}

func sqlExec(t *testing.T, db, sql string) {
	t.Helper()
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
}

func fileURI(p string) string { return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String() }

type agyFixture struct {
	home, root, dir, other string
}

// newAgyFixture: conversation A ran in dir (a path with a space), B in other,
// C has no workspace. A has its .db, a -wal and a brain directory.
func newAgyFixture(t *testing.T) *agyFixture {
	t.Helper()
	needSqlite3(t)
	f := &agyFixture{home: t.TempDir()}
	f.root = filepath.Join(f.home, ".gemini", "antigravity-cli")
	f.dir = filepath.Join(t.TempDir(), "work tree")
	f.other = filepath.Join(t.TempDir(), "other")
	for _, p := range []string{"conversations/A.db", "conversations/A.db-wal", "conversations/B.db", "brain/A/notes/plan.md"} {
		writeAgy(t, filepath.Join(f.root, filepath.FromSlash(p)), "x"+p)
	}
	db := filepath.Join(f.root, "conversation_summaries.db")
	sqlExec(t, db, agySchema+
		`INSERT INTO conversation_summaries (conversation_id, last_modified_time, workspace_uris, last_user_input_time) VALUES `+
		`('A', '2026-11-13 08:45:44.339760715+00:00', '["`+fileURI(f.dir)+`"]', '2026-11-13 08:45:41+00:00'),`+
		`('B', '2026-11-12 01:00:00+00:00', '["`+fileURI(f.other)+`"]', '2026-11-12 01:00:00+00:00'),`+
		`('C', '2026-11-11 01:00:00+00:00', '', '2026-11-11 01:00:00+00:00');`)
	return f
}

func writeAgy(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *agyFixture) store(t *testing.T) Store {
	t.Helper()
	st, err := New("agy", env(map[string]string{"HOME": f.home, "USERPROFILE": f.home}))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// agy's index names each conversation's workspace as a file URI (B-F10): a
// conversation is listed for the directory its URI decodes to, with the
// index's time and the size of all its files.
func TestAgySessionsByWorkspace(t *testing.T) {
	f := newAgyFixture(t)
	ss, err := f.store(t).Sessions(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != "A" || ss[0].Agent != "agy" {
		t.Fatalf("sessions %+v, want A alone", ss)
	}
	want := time.Date(2026, 11, 13, 8, 45, 44, 339760715, time.UTC)
	if !ss[0].Updated.Equal(want) || !ss[0].SizeKnown {
		t.Fatalf("updated %v size_known %v, want %v", ss[0].Updated, ss[0].SizeKnown, want)
	}
	var size int64
	for _, p := range []string{"conversations/A.db", "conversations/A.db-wal", "brain/A/notes/plan.md"} {
		size += int64(len("x" + p))
	}
	if ss[0].Size != size {
		t.Fatalf("size %d, want %d (the .db, its -wal and brain/)", ss[0].Size, size)
	}
}

// A session is its .db, the -wal / -shm beside it (without which recent writes
// are missing), and brain/<id>/ (B-F1a); only those names open.
func TestAgyFilesIncludeWalAndBrain(t *testing.T) {
	f := newAgyFixture(t)
	st := f.store(t)
	fs, err := st.Files(f.dir, "A")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range fs {
		names = append(names, x.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "brain/A/notes/plan.md,conversations/A.db,conversations/A.db-wal" {
		t.Fatalf("files %v", names)
	}
	rc, err := st.Open(f.dir, "A", "conversations/A.db-wal")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "xconversations/A.db-wal" {
		t.Fatalf("read %q", b)
	}
	for _, name := range []string{"conversations/B.db", "../x", "conversation_summaries.db"} {
		if _, err := st.Open(f.dir, "A", name); !errors.Is(err, ErrNoFile) {
			t.Fatalf("open %q: %v, want ErrNoFile", name, err)
		}
	}
	if _, err := st.Files(f.dir, "B"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("another directory's conversation: %v, want ErrNoSession", err)
	}
}

// agy never run on this host: nothing to list, not an error.
func TestAgyNeverRunIsEmpty(t *testing.T) {
	needSqlite3(t)
	st, err := New("agy", env(map[string]string{"HOME": t.TempDir(), "USERPROFILE": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if ss, err := st.Sessions(t.TempDir()); err != nil || len(ss) != 0 {
		t.Fatalf("%v %v, want nothing", ss, err)
	}
}

// agy's store present but laid out differently from what this runner knows —
// the index gone, or its column renamed — is an error that names itself, not
// an empty list: an empty list would hide that the runner needs updating.
func TestAgyLayoutChangeIsAnError(t *testing.T) {
	f := newAgyFixture(t)
	db := filepath.Join(f.root, "conversation_summaries.db")
	sqlExec(t, db, "ALTER TABLE conversation_summaries RENAME COLUMN workspace_uris TO workspaces;")
	if _, err := f.store(t).Sessions(f.dir); err == nil || !strings.Contains(err.Error(), "workspace_uris") {
		t.Fatalf("renamed column: %v, want an error naming it", err)
	}
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store(t).Sessions(f.dir); err == nil || !strings.Contains(err.Error(), "conversation_summaries.db") {
		t.Fatalf("index gone: %v, want an error naming it", err)
	}
}

// The index is read with the sqlite3 CLI (operator, 2026-11-15); a host
// without it says so.
func TestAgyWithoutSqlite3IsAnError(t *testing.T) {
	f := newAgyFixture(t)
	old := sqlite3Bin
	sqlite3Bin = filepath.Join(t.TempDir(), "no-sqlite3")
	t.Cleanup(func() { sqlite3Bin = old })
	if _, err := f.store(t).Sessions(f.dir); err == nil || !strings.Contains(err.Error(), "sqlite3") {
		t.Fatalf("%v, want an error naming sqlite3", err)
	}
}

// agy writes a zero last_modified_time for some conversations (17 of 263 on
// the measured host); those take their files' latest mtime instead of
// showing year 1.
func TestAgyZeroIndexTimeFallsBackToFiles(t *testing.T) {
	f := newAgyFixture(t)
	db := filepath.Join(f.root, "conversation_summaries.db")
	sqlExec(t, db, `UPDATE conversation_summaries SET last_modified_time = '0001-01-01 00:00:00+00:00' WHERE conversation_id = 'A';`)
	mt := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{"conversations/A.db", "conversations/A.db-wal", "brain/A/notes/plan.md"} {
		if err := os.Chtimes(filepath.Join(f.root, filepath.FromSlash(p)), mt, mt.Add(-time.Duration(len(p))*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	ss, err := f.store(t).Sessions(f.dir)
	if err != nil || len(ss) != 1 {
		t.Fatalf("%v %v", ss, err)
	}
	want := mt.Add(-time.Duration(len("conversations/A.db")) * time.Minute) // the newest of the three
	if !ss[0].Updated.Equal(want) {
		t.Fatalf("updated %v, want %v", ss[0].Updated, want)
	}
}
