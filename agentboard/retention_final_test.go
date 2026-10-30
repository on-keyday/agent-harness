package agentboard

// Regression tests for the final whole-branch review (I1, I2, M1).

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// I1: a retention can only add to a topic's life. Re-retaining with a period
// shorter than the board TTL, then letting it run out, drops the disk copy and
// leaves the messages to the board's own TTL — it never deletes them.
func TestRetainFinal_ShortTTLDoesNotDestroy(t *testing.T) {
	b, s := newRetainBoard(t, Config{TopicTTL: time.Hour}, t.TempDir())
	defer b.Close()
	defer s.Close()
	_ = b.Retain("op", Retention{Forever: true})
	publish(t, b, "op", "operator-kept")
	if err := b.Retain("op", Retention{TTL: time.Millisecond, SetAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	b.evictExpiredTopics()
	msgs, ok := b.ListRetained("op")
	if !ok || len(msgs) != 1 {
		t.Fatalf("a 1ms retention destroyed the topic (ok=%v, %d msgs); it may only stop keeping it", ok, len(msgs))
	}
	if _, _, retained := b.RetentionOf("op"); retained {
		t.Fatal("the expired retention is still set")
	}
}

// I2: loading a large retained log holds only the ring's payloads, not every
// payload in the file at once.
func TestRetainFinal_LoadMemoryBounded(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("big", Retention{Forever: true})
	payload := strings.Repeat("x", 1<<20)
	const n = 40
	for seq := uint64(1); seq <= n; seq++ {
		_ = s.Append("big", storeMsg(seq, payload))
	}
	_ = s.Close()

	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	peak := heapPeakDuring(t, func() {
		topics, err := s2.Load(2)
		if err != nil || len(topics) != 1 || len(topics[0].Ring) != 2 {
			t.Errorf("Load = %d topics, err %v", len(topics), err)
		}
	})
	if peak > 24<<20 {
		t.Fatalf("Load peaked at %d MiB of live heap for a %d MiB log with a 2-entry ring", peak>>20, n)
	}
	peak = heapPeakDuring(t, func() {
		if page, err := s2.Page("big", 0, 2); err != nil || len(page) != 2 {
			t.Errorf("Page = %d rows, err %v", len(page), err)
		}
	})
	if peak > 24<<20 {
		t.Fatalf("Page(2) peaked at %d MiB of live heap", peak>>20)
	}
	peak = heapPeakDuring(t, func() {
		if removed, err := s2.Remove("big", 20); err != nil || !removed {
			t.Errorf("Remove = %v, %v", removed, err)
		}
	})
	if peak > 24<<20 {
		t.Fatalf("Remove peaked at %d MiB of live heap", peak>>20)
	}
}

// heapPeakDuring samples live heap (after a GC at the start) while fn runs.
func heapPeakDuring(t *testing.T, fn func()) uint64 {
	t.Helper()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > base.HeapAlloc && ms.HeapAlloc-base.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc - base.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	fn()
	close(stop)
	<-done
	return peak.Load()
}

// M1: wait, like inbox and the hook, does not hand a sender its own
// --exclude-self message.
func TestExcludeSelf_WaitSkipsOwnRow(t *testing.T) {
	b, a, _, _ := newExcludeBoard(t, "grp")
	if _, _, err := b.Send("grp", []byte("mine"), exclRid, exclTidA, "h", "", 0, WithExcludeSelf()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	msgs, timedOut, err := b.Wait(ctx, a, "grp", 0, 0)
	if err != nil || !timedOut || len(msgs) != 0 {
		t.Fatalf("Wait = %d msgs, timedOut %v, err %v; want a timeout with nothing", len(msgs), timedOut, err)
	}
}
