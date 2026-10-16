package agentboard

import (
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// taskState is per-(runner_id, task_id) persistent state shared across all
// ConnStates of the same task. Lifetime: lazily created on the first
// Board.Attach for the (rid, tid) pair, destroyed when the ticket is revoked
// (TaskFinished) via Board.Revoke. This is what makes subscriptions survive
// the short-lived per-subcommand harness-cli connections — without it, every
// `harness-cli agent` invocation would start with an empty subscription set
// and inbox would always be empty.
type taskState struct {
	mu       sync.Mutex
	patterns map[string]struct{}
	// waiting counts the live synchronous Board.Wait calls per topic for this
	// task. It does two jobs, and they have the same extent: a topic with a
	// live wait is subscribed for the duration of that wait, and a publish to
	// it must not also wake this task's PTY — the waiter is already being
	// handed the message, so the wake would type a prompt into an agent about
	// something it did not ask for. Refcounted because two waits on one topic
	// may overlap; the entry is deleted at zero so matches needs no zero check.
	waiting map[string]int
	// shown is the highest seq per topic that the automatic injection path has
	// already handed to this task. Only Board.InboxAdvance writes it; Inbox
	// never touches it. Per topic rather than one scalar per task, because a
	// reader covering ONE topic must not be able to claim progress on the
	// others — that is exactly what the client-side cursor got wrong.
	shown map[string]uint64
	// contacts are reply routes of messages handed to this task (reach spec
	// §3): the right to answer someone who spoke first, kept here rather than
	// in the ring so a rotated-out message does not take it along. Bounded,
	// because a sender picks ReplyToTopic freely; the least recently added
	// goes first, and a re-add counts as an add, so a correspondent who keeps
	// writing is not the one dropped. contactOrder runs oldest to newest.
	contacts     map[string]struct{}
	contactOrder []string
	conns        map[*ConnState]struct{}
	rid          protocol.RunnerID
	tid          protocol.TaskID
	host         string
	profile      string
}

func newTaskState() *taskState {
	return &taskState{
		patterns: make(map[string]struct{}),
		waiting:  make(map[string]int),
		shown:    make(map[string]uint64),
		contacts: make(map[string]struct{}),
		conns:    make(map[*ConnState]struct{}),
	}
}

const maxContacts = 64

func (t *taskState) addContact(topic string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.contacts[topic]; ok {
		for i, c := range t.contactOrder {
			if c == topic {
				t.contactOrder = append(t.contactOrder[:i], t.contactOrder[i+1:]...)
				break
			}
		}
		t.contactOrder = append(t.contactOrder, topic)
		return
	}
	if len(t.contactOrder) == maxContacts {
		delete(t.contacts, t.contactOrder[0])
		t.contactOrder = t.contactOrder[1:]
	}
	t.contacts[topic] = struct{}{}
	t.contactOrder = append(t.contactOrder, topic)
}

// snapshotContacts copies the contacts newest first.
func (t *taskState) snapshotContacts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.contactOrder))
	for i, c := range t.contactOrder {
		out[len(out)-1-i] = c
	}
	return out
}

func (t *taskState) hasContact(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.contacts[topic]
	return ok
}

// takeUnshown records msgs as shown for topic and returns the ones that were
// above the mark. Collecting and marking happen under one acquisition of t.mu,
// so two concurrent advances cannot both return the same message.
func (t *taskState) takeUnshown(topic string, msgs []RetainedMessage) []RetainedMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	mark := t.shown[topic]
	out := make([]RetainedMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq <= mark {
			continue
		}
		out = append(out, m)
		if m.Seq > t.shown[topic] {
			t.shown[topic] = m.Seq
		}
	}
	return out
}

// shownSnapshot copies the per-topic marks, for the operator view.
func (t *taskState) shownSnapshot() map[string]uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]uint64, len(t.shown))
	for k, v := range t.shown {
		out[k] = v
	}
	return out
}

// beginWait registers a live synchronous wait on topic. Paired with endWait.
func (t *taskState) beginWait(topic string) {
	t.mu.Lock()
	t.waiting[topic]++
	t.mu.Unlock()
}

// endWait retires one live wait on topic. Deleting at zero keeps matches a
// plain map lookup rather than a lookup plus a count comparison.
func (t *taskState) endWait(topic string) {
	t.mu.Lock()
	if n := t.waiting[topic] - 1; n > 0 {
		t.waiting[topic] = n
	} else {
		delete(t.waiting, topic)
	}
	t.mu.Unlock()
}

// isWaiting reports whether this task has a live synchronous wait on topic.
func (t *taskState) isWaiting(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.waiting[topic]
	return ok
}

func (t *taskState) addPattern(p string) {
	t.mu.Lock()
	t.patterns[p] = struct{}{}
	t.mu.Unlock()
}

func (t *taskState) removePattern(p string) {
	t.mu.Lock()
	delete(t.patterns, p)
	t.mu.Unlock()
}

// matches reports whether a publish to topic reaches this task. It is the
// union of the persistent subscription set and the topics under a live wait:
// waiting on a topic subscribes to it for exactly the duration of the wait,
// and leaves nothing behind afterwards.
func (t *taskState) matches(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.patterns[topic]; ok {
		return true
	}
	_, ok := t.waiting[topic]
	return ok
}

func (t *taskState) attachConn(c *ConnState) {
	t.mu.Lock()
	t.conns[c] = struct{}{}
	t.mu.Unlock()
}

func (t *taskState) detachConn(c *ConnState) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

// snapshotPatterns returns the union matches tests against: the persistent
// subscriptions plus any topic under a live wait, each name once. Feeding
// Inbox a duplicated name would return that topic's ring twice.
func (t *taskState) snapshotPatterns() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.patterns)+len(t.waiting))
	for p := range t.patterns {
		out = append(out, p)
	}
	for p := range t.waiting {
		if _, dup := t.patterns[p]; !dup {
			out = append(out, p)
		}
	}
	return out
}

func (t *taskState) snapshotConns() []*ConnState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*ConnState, 0, len(t.conns))
	for c := range t.conns {
		out = append(out, c)
	}
	return out
}

func (t *taskState) setIdentity(rid protocol.RunnerID, tid protocol.TaskID, hostname, agentProfile string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rid = rid
	t.tid = tid
	t.host = hostname
	t.profile = agentProfile
}

func (t *taskState) identity() (protocol.RunnerID, protocol.TaskID, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rid, t.tid, t.host, t.profile
}
