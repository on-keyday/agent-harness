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
// prints list, `export <id>` prints "EXPORT <id>" (or fails for "bad"), and
// every call appends its argv and cwd to a log.
func fakeOpencode(t *testing.T, list string) (logPath string) {
	t.Helper()
	d := t.TempDir()
	logPath = filepath.Join(d, "calls.log")
	script := `#!/bin/sh
echo "$PWD|$*" >> '` + logPath + `'
if [ "$1" = session ]; then cat <<'JSON'
` + list + `
JSON
exit 0; fi
if [ "$1" = export ]; then
  if [ "$2" = bad ]; then echo boom >&2; exit 3; fi
  echo "EXPORT $2"; exit 0
fi
exit 9
`
	bin := filepath.Join(d, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := opencodeBin
	opencodeBin = bin
	t.Cleanup(func() { opencodeBin = old })
	return logPath
}

func opencodeList(dir, other string) string {
	return `[{"id":"ses_a","directory":"` + dir + `","created":1787143409223,"updated":1787143560785,"projectId":"p","title":"t"},` +
		`{"id":"ses_b","directory":"` + other + `","created":1,"updated":2,"projectId":"p","title":"t"}]`
}

// opencode lists itself (B-F9): rows whose directory is the task's, with the
// times it reports (unix ms).
func TestOpencodeListsByDirectory(t *testing.T) {
	dir := t.TempDir()
	logPath := fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	st, err := New("opencode", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != "ses_a" || ss[0].Agent != "opencode" {
		t.Fatalf("sessions %+v, want ses_a", ss)
	}
	if want := time.UnixMilli(1787143409223); !ss[0].Created.Equal(want) {
		t.Fatalf("created %v, want %v", ss[0].Created, want)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), dir+"|session list --format json") {
		t.Fatalf("calls %q, want session list run in the task dir", calls)
	}
}

// The worktree may be gone; opencode is then asked from the nearest directory
// that still exists, which is in the same project (the repo).
func TestOpencodeListsFromNearestExistingDir(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".harness-worktrees", "gone")
	logPath := fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	st, _ := New("opencode", env(nil))
	ss, err := st.Sessions(dir)
	if err != nil || len(ss) != 1 {
		t.Fatalf("sessions %+v err %v", ss, err)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.HasPrefix(string(calls), repo+"|") {
		t.Fatalf("calls %q, want run from %s", calls, repo)
	}
}

// One session is one file, produced on read by `opencode export <id>` (B-D4).
func TestOpencodeGetRunsExport(t *testing.T) {
	dir := t.TempDir()
	fakeOpencode(t, opencodeList(dir, "/elsewhere"))
	st, _ := New("opencode", env(nil))
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
	if err != nil || strings.TrimSpace(string(b)) != "EXPORT ses_a" {
		t.Fatalf("read %q err %v", b, err)
	}
	if _, err := st.Files(dir, "ses_b"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a session of another directory: err %v, want ErrNoSession", err)
	}
	if _, err := st.Open(dir, "ses_a", "../x"); !errors.Is(err, ErrNoFile) {
		t.Fatalf("an unlisted name: err %v, want ErrNoFile", err)
	}
}

// A failing export is an error at the end of the read, not a short file
// passed off as complete.
func TestOpencodeExportFailureIsStoreError(t *testing.T) {
	dir := t.TempDir()
	fakeOpencode(t, `[{"id":"bad","directory":"`+dir+`","created":1,"updated":2}]`)
	st, _ := New("opencode", env(nil))
	rc, err := st.Open(dir, "bad", "export.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v, want the export's failure with its stderr", err)
	}
}
