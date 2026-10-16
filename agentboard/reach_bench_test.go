package agentboard

// The two board paths a per-read reach check would sit on, measured as they
// are today: the publish fan-out over S subscribers, and an inbox read
// returning R rows. The check itself is benchmarked in server/reach_bench_test.go
// against a real TaskStore; the cost with it in place is this baseline plus one
// check per subscriber (fan-out) or per subscribed topic (inbox).
// See docs/superpowers/specs/2026-10-16-agentboard-reach-design.md §6.
//
//	go test ./agentboard/ -run '^$' -bench 'BenchmarkReachBase' -benchmem -count 6

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func reachBenchBoard(b *testing.B) *Board {
	b.Helper()
	board := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 1024, MaxPayload: 1 << 20})
	b.Cleanup(func() { board.Close() })
	return board
}

func reachBenchTask(i int) protocol.TaskID {
	var tid protocol.TaskID
	binary.BigEndian.PutUint32(tid.Id[:], uint32(i+1))
	return tid
}

func BenchmarkReachBaseSendFanout(b *testing.B) {
	for _, s := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("S=%d", s), func(b *testing.B) {
			board := reachBenchBoard(b)
			var rid protocol.RunnerID
			for i := 0; i < s; i++ {
				c := board.Attach(rid, reachBenchTask(i), "h", "bash")
				if err := board.Subscribe(c, "bench.topic"); err != nil {
					b.Fatal(err)
				}
			}
			payload := []byte("x")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := board.Send("bench.topic", payload, rid, reachBenchTask(-1), "h", "", 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Inbox, not InboxAdvance: the same pattern iteration and ring reads, but
// idempotent, so every iteration returns all R rows without republishing.
func BenchmarkReachBaseInbox(b *testing.B) {
	for _, r := range []int{1, 64} {
		b.Run(fmt.Sprintf("R=%d", r), func(b *testing.B) {
			board := reachBenchBoard(b)
			var rid protocol.RunnerID
			c := board.Attach(rid, reachBenchTask(0), "h", "bash")
			if err := board.Subscribe(c, "bench.topic"); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < r; i++ {
				if _, _, err := board.Send("bench.topic", []byte("x"), rid, reachBenchTask(-1), "h", "", 0, WithNoWake()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if msgs, _ := board.Inbox(c, 0); len(msgs) != r {
					b.Fatalf("inbox returned %d rows, want %d", len(msgs), r)
				}
			}
		})
	}
}
