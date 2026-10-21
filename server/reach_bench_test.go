package server

// What a per-read reach check costs, before one is built: the measurement
// docs/superpowers/specs/2026-10-16-agentboard-reach-design.md §6 makes D10
// conditional on. The check below is a PROTOTYPE of §5's read predicate and
// §2's publish base set, written against the real TaskStore so its lookups pay
// the store's lock and its hex-keyed map, as a real one would. It is not wired
// into the board; agentboard/reach_bench_test.go measures the paths it would
// sit on.
//
//	go test ./server/ -run '^$' -bench 'BenchmarkReach' -benchmem -count 6

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// reachStore holds n tasks, d of them a creator chain root -> ... -> leaf; the
// rest are operator-rooted bystanders, which only make the store larger.
type reachFixture struct {
	s          *TaskStore
	root, leaf protocol.TaskID
	stranger   protocol.TaskID // in the store, outside the chain
}

func reachStore(b *testing.B, n, d int) reachFixture {
	b.Helper()
	s := NewTaskStore()
	create := func(creator protocol.TaskID) protocol.TaskID {
		idHex := s.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
			creator, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, authority.Scope{}, "bash")
		raw, err := hex.DecodeString(idHex)
		if err != nil {
			b.Fatal(err)
		}
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		return tid
	}
	var f reachFixture
	parent := protocol.TaskID{}
	for i := 0; i < d; i++ {
		parent = create(parent)
		if i == 0 {
			f.root = parent
		}
	}
	f.leaf = parent
	for i := d; i < n; i++ {
		f.stranger = create(protocol.TaskID{})
	}
	f.s = s
	return f
}

// The two lookups the predicate needs, read under the store's own lock.
func (f reachFixture) creatorOf(id protocol.TaskID) (protocol.TaskID, bool) {
	f.s.mu.RLock()
	defer f.s.mu.RUnlock()
	e, ok := f.s.tasks[hex.EncodeToString(id.Id[:])]
	if !ok {
		return protocol.TaskID{}, false
	}
	return e.CreatorTaskID, true
}

func (f reachFixture) capsOf(id protocol.TaskID) protocol.Capability {
	f.s.mu.RLock()
	defer f.s.mu.RUnlock()
	if e, ok := f.s.tasks[hex.EncodeToString(id.Id[:])]; ok {
		return e.Capabilities
	}
	return protocol.Capability_None
}

// member is §4: x is caller or on caller's creator chain. Stops at the root, a
// missing record (a prune hole ends membership, D9), or after len(store) steps.
func (f reachFixture) member(caller, x protocol.TaskID) bool {
	limit := len(f.s.tasks)
	cur := caller
	for step := 0; step <= limit; step++ {
		if cur == x {
			return true
		}
		next, ok := f.creatorOf(cur)
		if !ok || next == (protocol.TaskID{}) {
			return false
		}
		cur = next
	}
	return false
}

// readable is §5's predicate.
func (f reachFixture) readable(caller protocol.TaskID, topic string) bool {
	switch {
	case strings.HasPrefix(topic, agentboard.SelfTopicPrefix):
		rest := topic[len(agentboard.SelfTopicPrefix):]
		if rest != "operator" {
			short := rest
			if i := strings.IndexByte(rest, '.'); i >= 0 {
				short = rest[:i]
			}
			if agentboard.SelfTopicPrefix+short == agentboard.SelfTopic(caller) {
				return true
			}
		}
		return f.capsOf(caller)&protocol.Capability_BoardObserve != 0
	case strings.HasPrefix(topic, "tree."):
		rest := topic[len("tree."):]
		if len(rest) < 33 || rest[32] != '.' {
			return true // not a tree namespace: a free-form name
		}
		raw, err := hex.DecodeString(rest[:32])
		if err != nil {
			return true
		}
		var x protocol.TaskID
		copy(x.Id[:], raw)
		if f.member(caller, x) {
			return true
		}
		return f.capsOf(caller)&protocol.Capability_BoardObserve != 0
	default:
		return true
	}
}

// sendBase is §2's base set for a chat.<short> destination: self, the
// creator, or a direct child. A child is found by scanning the store for a task
// whose creator is the caller — the spec's "SelfTopic(C) for every live task C
// whose CreatorTaskID is T", taken literally.
func (f reachFixture) sendBase(caller protocol.TaskID, topic string) bool {
	if topic == agentboard.SelfTopic(caller) || topic == agentboard.OperatorTopic {
		return true
	}
	if c, ok := f.creatorOf(caller); ok && c != (protocol.TaskID{}) && topic == agentboard.SelfTopic(c) {
		return true
	}
	short, ok := strings.CutPrefix(topic, agentboard.SelfTopicPrefix)
	if !ok || len(short) != 8 {
		return false
	}
	f.s.mu.RLock()
	defer f.s.mu.RUnlock()
	for _, e := range f.s.tasks {
		if e.CreatorTaskID == caller && strings.HasPrefix(e.ID, short) {
			return true
		}
	}
	return false
}

var reachSink bool

func reachSizes(yield func(n, d int)) {
	for _, n := range []int{10, 100, 1000, 10000} {
		for _, d := range []int{1, 4, 16} {
			if d <= n {
				yield(n, d)
			}
		}
	}
}

func BenchmarkReachRead(b *testing.B) {
	reachSizes(func(n, d int) {
		f := reachStore(b, n, d)
		cases := []struct {
			name, topic string
			want        bool
		}{
			{"free", "build.events", true},
			{"chat-self", agentboard.SelfTopic(f.leaf) + ".side", true},
			{"chat-other", agentboard.SelfTopic(f.stranger), false},
			// The worst member: the root, d hops up.
			{"tree-member", "tree." + hex.EncodeToString(f.root.Id[:]) + ".x", true},
			// A miss walks the whole chain and then reads caps.
			{"tree-nonmember", "tree." + hex.EncodeToString(f.stranger.Id[:]) + ".x", false},
		}
		for _, c := range cases {
			if n == 10 && d == 16 {
				continue
			}
			b.Run(fmt.Sprintf("N=%d/d=%d/%s", n, d, c.name), func(b *testing.B) {
				if got := f.readable(f.leaf, c.topic); got != c.want {
					b.Fatalf("readable(%s) = %v, want %v", c.topic, got, c.want)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					reachSink = f.readable(f.leaf, c.topic)
				}
			})
		}
	})
}

// The same check from many goroutines: every lookup takes the store's read
// lock, which is shared with every other request the server is serving.
func BenchmarkReachReadParallel(b *testing.B) {
	for _, n := range []int{100, 10000} {
		f := reachStore(b, n, 4)
		topic := "tree." + hex.EncodeToString(f.root.Id[:]) + ".x"
		b.Run(fmt.Sprintf("N=%d/d=4/tree-member", n), func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if !f.readable(f.leaf, topic) {
						b.Error("member read refused")
						return
					}
				}
			})
		})
	}
}

// D8's alternative, for the number it is rejected on: membership answered by
// building the child index (server/capabilities.go childIndex) and walking the
// subtree down from x.
func BenchmarkReachMembershipChildIndex(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		f := reachStore(b, n, 4)
		h := &TaskHandler{Tasks: f.s}
		rootHex := hex.EncodeToString(f.root.Id[:])
		leafHex := hex.EncodeToString(f.leaf.Id[:])
		b.Run(fmt.Sprintf("N=%d/d=4", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				idx := h.childIndex()
				found := false
				stack := []string{rootHex}
				for len(stack) > 0 && !found {
					cur := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if cur == leafHex {
						found = true
					}
					stack = append(stack, idx[cur]...)
				}
				reachSink = found
			}
		})
	}
}

// §2's base set, once per publish. "child" is the scan's worst case: the
// destination is the caller's child, found only by visiting the store.
func BenchmarkReachSendBase(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		f := reachStore(b, n, 4)
		// The leaf's creator publishing to the leaf: a child destination.
		parent, _ := f.creatorOf(f.leaf)
		cases := []struct {
			name, topic string
			caller      protocol.TaskID
			want        bool
		}{
			{"creator", agentboard.SelfTopic(parent), f.leaf, true},
			{"child", agentboard.SelfTopic(f.leaf), parent, true},
			{"miss", agentboard.SelfTopic(f.stranger), f.leaf, false},
		}
		for _, c := range cases {
			b.Run(fmt.Sprintf("N=%d/%s", n, c.name), func(b *testing.B) {
				if got := f.sendBase(c.caller, c.topic); got != c.want {
					b.Fatalf("sendBase(%s) = %v, want %v", c.topic, got, c.want)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					reachSink = f.sendBase(c.caller, c.topic)
				}
			})
		}
	}
}
