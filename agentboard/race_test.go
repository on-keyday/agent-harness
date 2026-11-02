package agentboard

import (
	"sync"
	"testing"
)

// A flush racing a --clear must not lose what the ring held: whichever runs
// first, the topic ends unretained with its newest messages in the ring.
func TestFlush_RacingClearLosesNothing(t *testing.T) {
	for i := 0; i < 40; i++ {
		b, s := newRetainBoard(t, Config{}, t.TempDir())
		seqs := retainedWith100(t, b, "g", Retention{Forever: true})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _, _ = b.Flush("g") }()
		go func() { defer wg.Done(); _ = b.ClearRetain("g") }()
		wg.Wait()
		live, _ := b.ListRetained("g")
		if len(live) < 63 || live[len(live)-1].Seq != seqs[98] {
			t.Fatalf("iteration %d: ring after flush || clear = %d rows, want the newest live ending at %d", i, len(live), seqs[98])
		}
		if b.RingFloor("g") != 0 {
			t.Fatalf("iteration %d: a floor survived on an unretained topic", i)
		}
		b.Close()
		_ = s.Close()
	}
}

// Concurrent publishes racing an archive: every seq at or below the
// archive's through is in it.
func TestBoardArchive_RacingPublishesAllOrNothing(t *testing.T) {
	for i := 0; i < 20; i++ {
		b, s := newRetainBoard(t, Config{}, t.TempDir())
		if err := b.Retain("g", Retention{Forever: true}); err != nil {
			t.Fatal(err)
		}
		publish(t, b, "g", "seed")
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 25; j++ {
					_, _, _ = b.Send("g", []byte("m"), retRid, retTid, "h", "", 0)
				}
			}()
		}
		info, err := b.Archive("g", retTid)
		wg.Wait()
		if err != nil {
			t.Fatal(err)
		}
		arch, _ := b.ArchivePage(info.ID, 0, 1000)
		in := map[uint64]bool{}
		for _, m := range arch {
			in[m.Seq] = true
		}
		all, _ := b.Page("g", 0, 1000)
		for _, m := range all {
			if m.Seq <= info.Through && !in[m.Seq] {
				t.Fatalf("iteration %d: seq %d <= through %d is missing from the archive", i, m.Seq, info.Through)
			}
		}
		b.Close()
		_ = s.Close()
	}
}
