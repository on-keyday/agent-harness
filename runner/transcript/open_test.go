//go:build !windows

package transcript

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An agent can write into its own store (a sandbox mounts it read-write). A
// file there that is a symlink is never listed and never opened: following it
// would hand out whatever it points at on the runner host (review of Tasks
// 2–4, C1).
func TestSymlinkedFileIsNotListedOrOpened(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "id_ed25519")
	write(t, secret, "SECRET")
	dir := "/w/r/.harness-worktrees/link"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	write(t, filepath.Join(proj, "s1.jsonl"), `{"cwd":"`+dir+`"}`+"\n")
	if err := os.MkdirAll(filepath.Join(proj, "s1", "tool-results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(proj, "s1", "tool-results", "t.txt")); err != nil {
		t.Fatal(err)
	}
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}))
	fs, err := st.Files(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if strings.Contains(f.Name, "t.txt") {
			t.Fatalf("a symlink was listed: %+v", fs)
		}
	}
	if rc, err := st.Open(dir, "s1", "s1/tool-results/t.txt"); !errors.Is(err, ErrNoFile) {
		if rc != nil {
			b, _ := io.ReadAll(rc)
			rc.Close()
			t.Fatalf("opened a symlink, read %q", b)
		}
		t.Fatalf("err %v, want ErrNoFile", err)
	}
}

// The race the review measured: the file is listed as a regular file, then
// swapped for a symlink before it is opened. Whatever wins, the target's
// bytes never come back.
func TestSymlinkSwapWhileOpeningNeverReadsTheTarget(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	write(t, secret, "SECRET")
	dir := "/w/r/.harness-worktrees/race"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	main := filepath.Join(proj, "s1.jsonl")
	body := `{"cwd":"` + dir + `"}` + "\n"
	write(t, main, body)
	regular := filepath.Join(proj, "regular.tmp")
	link := filepath.Join(proj, "link.tmp")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				_ = os.WriteFile(regular, []byte(body), 0o644)
				_ = os.Rename(regular, main)
			} else {
				_ = os.Remove(link)
				_ = os.Symlink(secret, link)
				_ = os.Rename(link, main)
			}
		}
	}()
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}))
	for i := 0; i < 3000; i++ {
		rc, err := st.Open(dir, "s1", "s1.jsonl")
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(b), "SECRET") {
			close(stop)
			<-done
			t.Fatalf("read the symlink target on try %d", i)
		}
	}
	close(stop)
	<-done
}

// A FIFO or a symlink to a device planted as a session file is skipped: read,
// it would block or never end (review of Tasks 2–4, I2).
func TestSessionsSkipFifoAndDevices(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/fifo"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	write(t, filepath.Join(proj, "ok.jsonl"), `{"cwd":"`+dir+`"}`+"\n")
	if err := syscall.Mkfifo(filepath.Join(proj, "fifo.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(proj, "zero.jsonl")); err != nil {
		t.Fatal(err)
	}
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}))
	got := make(chan []Session, 1)
	go func() {
		ss, _ := st.Sessions(dir)
		got <- ss
	}()
	select {
	case ss := <-got:
		if len(ss) != 1 || ss[0].ID != "ok" {
			t.Fatalf("sessions %+v, want only ok", ss)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Sessions blocked on a planted FIFO or device")
	}
}

// A regular file with no newline for a very long way is not read to its end
// to find a header.
func TestHeaderReadIsBounded(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/huge"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	p := filepath.Join(proj, "big.jsonl")
	write(t, p, "")
	if err := os.Truncate(p, 200<<20); err != nil { // 200 MiB of NULs, no newline
		t.Fatal(err)
	}
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}))
	start := time.Now()
	if _, err := st.Sessions(dir); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("reading for a header took %v: unbounded", d)
	}
}
