//go:build !windows

package transcript

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOpencode installs a shell script as the opencode binary: `session list`
// prints list, `session export <id>` (the id last) prints {"id":"<id>"}
// (fails for "bad", prints a cut-off value for "notjson"), and every call
// appends its argv and cwd to a log. A call without --standalone fails: on
// opencode 2.x it would start or join the shared background service. Like
// opencode 1.18.18, which ended a piped export at 128KiB and exited 0, the
// fake prints only a fragment when its stdout is a pipe.
func fakeOpencode(t *testing.T, list string) (logPath, bin string) {
	t.Helper()
	d := t.TempDir()
	logPath = filepath.Join(d, "calls.log")
	script := `#!/bin/sh
echo "$PWD|$*" >> '` + logPath + `'
case " $* " in *" --standalone "*) ;; *) echo "no --standalone" >&2; exit 8;; esac
if [ "$1 $2" = "session list" ]; then cat <<'JSON'
` + list + `
JSON
exit 0; fi
if [ "$1 $2" = "session export" ]; then
  for id; do :; done
  if [ "$id" = bad ]; then echo boom >&2; exit 3; fi
  if [ -p /dev/stdout ]; then printf '{"id":"%s","cu' "$id"; exit 0; fi
  if [ "$id" = notjson ]; then printf '{"id":'; exit 0; fi
  printf '{"id":"%s"}\n' "$id"; exit 0
fi
exit 9
`
	bin = filepath.Join(d, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return logPath, bin
}

func opencodeList(dir, other string) string {
	return `[{"id":"ses_a","directory":"` + dir + `","created":1787143409223,"updated":1787143560785,"projectId":"p","title":"t"},` +
		`{"id":"ses_b","directory":"` + other + `","created":1,"updated":2,"projectId":"p","title":"t"}]`
}

// opencode lists itself (B-F9): rows whose directory is the task's, with the
// times it reports (unix ms).
func TestOpencodeListsByDirectory(t *testing.T) {
	dir := t.TempDir()
	logPath, bin := fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	st, err := New("opencode", env(nil), bin)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != "ses_a" || ss[0].Agent != "opencode" || ss[0].SizeKnown {
		t.Fatalf("sessions %+v, want ses_a", ss)
	}
	if want := time.UnixMilli(1787143409223); !ss[0].Created.Equal(want) {
		t.Fatalf("created %v, want %v", ss[0].Created, want)
	}
	calls, _ := os.ReadFile(logPath)
	// -n: 2.x returns only the project's 100 newest sessions by default, and
	// every task worktree of a repo is one project, so the directory filter
	// above must see all of them.
	if !strings.Contains(string(calls), dir+"|session list --standalone --format json -n 2147483647") {
		t.Fatalf("calls %q, want an uncapped standalone session list run in the task dir", calls)
	}
}

// The worktree may be gone; opencode is then asked from the nearest directory
// that still exists, which is in the same project (the repo).
func TestOpencodeListsFromNearestExistingDir(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".harness-worktrees", "gone")
	logPath, bin := fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	st, _ := New("opencode", env(nil), bin)
	ss, err := st.Sessions(dir)
	if err != nil || len(ss) != 1 {
		t.Fatalf("sessions %+v err %v", ss, err)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.HasPrefix(string(calls), repo+"|") {
		t.Fatalf("calls %q, want run from %s", calls, repo)
	}
}

// One session is one file, produced on read by `opencode session export <id>`
// (B-D4).
func TestOpencodeGetRunsExport(t *testing.T) {
	dir := t.TempDir()
	_, bin := fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	st, _ := New("opencode", env(nil), bin)
	fs, err := st.Files(dir, "ses_a")
	if err != nil || len(fs) != 1 || fs[0].Name != "export.json" || fs[0].SizeKnown {
		t.Fatalf("files %+v err %v", fs, err)
	}
	rc, err := st.Open(dir, "ses_a", "export.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil || strings.TrimSpace(string(b)) != `{"id":"ses_a"}` {
		t.Fatalf("read %q err %v", b, err)
	}
	if _, err := st.Files(dir, "ses_b"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a session of another directory: err %v, want ErrNoSession", err)
	}
	if _, err := st.Open(dir, "ses_a", "../x"); !errors.Is(err, ErrNoFile) {
		t.Fatalf("an unlisted name: err %v, want ErrNoFile", err)
	}
}

// A failing export is an error from Open, not a short file passed off as
// complete.
func TestOpencodeExportFailureIsStoreError(t *testing.T) {
	dir := t.TempDir()
	_, bin := fakeOpencode(t, `[{"id":"bad","directory":"`+dir+`","created":1,"updated":2}]`)
	st, _ := New("opencode", env(nil), bin)
	rc, err := st.Open(dir, "bad", "export.json")
	if err == nil {
		rc.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v, want the export's failure with its stderr", err)
	}
}

// An export that exits 0 with less than one whole JSON value is an error too:
// the exit status alone did not catch opencode's cut-off output.
func TestOpencodeExportCutShortIsStoreError(t *testing.T) {
	dir := t.TempDir()
	_, bin := fakeOpencode(t, `[{"id":"notjson","directory":"`+dir+`","created":1,"updated":2}]`)
	st, _ := New("opencode", env(nil), bin)
	rc, err := st.Open(dir, "notjson", "export.json")
	if err == nil {
		rc.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("err %v, want an incomplete-JSON error", err)
	}
}

// With no binary given, the store runs the name `opencode` from PATH, as it
// did before a profile could name one.
func TestOpencodeWithoutABinUsesTheNameOnPath(t *testing.T) {
	dir := t.TempDir()
	logPath, bin := fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	st, _ := New("opencode", env(nil), "")
	if ss, err := st.Sessions(dir); err != nil || len(ss) != 1 {
		t.Fatalf("sessions %+v err %v", ss, err)
	}
	if calls, _ := os.ReadFile(logPath); len(calls) == 0 {
		t.Fatal("the opencode on PATH was not run")
	}
}
