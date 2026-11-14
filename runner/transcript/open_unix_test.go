//go:build unix

package transcript

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO renamed over a file between openRegular's Lstat and its open must
// not block the open: an agent can do this in its own store, and a blocked
// open hangs the request — and every listing on that runner — with no writer
// ever coming (review of pillar B Task 5, I1).
func TestFifoSwapWhileOpeningNeverBlocks(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "s1.jsonl")
	regular := filepath.Join(base, "regular.tmp")
	fifo := filepath.Join(base, "fifo.tmp")
	if err := os.WriteFile(main, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
				_ = os.WriteFile(regular, []byte("x\n"), 0o644)
				_ = os.Rename(regular, main)
			} else {
				_ = os.Remove(fifo)
				_ = syscall.Mkfifo(fifo, 0o644)
				_ = os.Rename(fifo, main)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 3000; i++ {
		res := make(chan struct{})
		go func() {
			defer close(res)
			if f, _, err := openRegular(base, "s1.jsonl"); err == nil {
				f.Close()
			}
		}()
		select {
		case <-res:
		case <-time.After(2 * time.Second):
			// Release the blocked open so the process can exit.
			if w, err := os.OpenFile(main, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
			}
			t.Fatalf("openRegular blocked on a FIFO swapped in on try %d", i)
		}
	}
}
