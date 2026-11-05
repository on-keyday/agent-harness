package agentboard

import (
	"slices"
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
	// waits holds the live synchronous Board.Wait calls per topic for this
	// task. A topic with a live wait is subscribed for the duration of that
	// wait (matches). It also decides the wake: see liveWait. A slice because
	// two waits on one topic may overlap; the entry is deleted when it empties
	// so matches is a plain map lookup.
	waits map[string][]*liveWait
	// The delivery mark is NOT here: it is topic.shown, so it lives as long as
	// the ring it indexes rather than as long as this state, which Revoke
	// destroys while the topic may stay.
	//
	// contacts are reply routes of messages handed to this task (reach spec
	// §3): the right to answer someone who spoke first, kept here rather than
	// in the ring so a rotated-out message does not take it along. Bounded,
	// because a sender picks ReplyToTopic freely; the least recently added
	// goes first, and a re-add counts as an add, so a correspondent who keeps
	// writing is not the one dropped. contactOrder runs oldest to newest.
	contacts     map[string]struct{}
	contactOrder []string
	// contactFloor and routeFloor keep a forgotten contact forgotten. A plain
	// inbox re-reads the whole ring, so the message that opened a route is
	// handed over again after the operator cleared it; only a message whose
	// seq is above the floor recorded at the clear may add it back.
	// contactFloor covers every route (clear all), routeFloor one each.
	contactFloor uint64
	routeFloor   map[string]uint64
	conns        map[*ConnState]struct{}
	rid          protocol.RunnerID
	tid          protocol.TaskID
	host         string
	profile      string
}

func newTaskState() *taskState {
	return &taskState{
		patterns: make(map[string]struct{}),
		waits:    make(map[string][]*liveWait),
		contacts: make(map[string]struct{}),
		conns:    make(map[*ConnState]struct{}),
	}
}

const maxContacts = 64

// addContact records topic as a contact, from a message with this seq.
func (t *taskState) addContact(topic string, seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.contactFloor != 0 && seq <= t.contactFloor {
		return
	}
	if f, ok := t.routeFloor[topic]; ok {
		if seq <= f {
			return
		}
		delete(t.routeFloor, topic)
	}
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

// forgetContacts removes topic from the contacts, or every contact when topic
// is empty, and reports how many went. floor is the board's last issued seq:
// a message at or below it cannot add the route back.
func (t *taskState) forgetContacts(topic string, floor uint64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if topic == "" {
		n := len(t.contactOrder)
		t.contacts = make(map[string]struct{})
		t.contactOrder = nil
		t.contactFloor = floor
		t.routeFloor = nil
		return n
	}
	if t.routeFloor == nil {
		t.routeFloor = make(map[string]uint64)
	}
	t.routeFloor[topic] = floor
	if _, ok := t.contacts[topic]; !ok {
		return 0
	}
	delete(t.contacts, topic)
	for i, c := range t.contactOrder {
		if c == topic {
			t.contactOrder = append(t.contactOrder[:i], t.contactOrder[i+1:]...)
			break
		}
	}
	return 1
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

// liveWait is one synchronous Board.Wait in progress.
//
// A publish to a topic this task waits on must not also wake the task's PTY
// when the wait hands the message over: the wake would type a
// <harness:agentboard-wake> prompt into an agent sharing the task id, about a
// message it did not ask for and the waiter has already taken. Whether the
// wait hands it over is known for sure only when the wait returns, so the
// publish does not decide it alone:
//
//   - Board.Send calls handOff under the topic's lock, before any wait can
//     see the message. A wait whose filter (wants) accepts it records the seq
//     in owed, and the publish emits no wake for this task.
//   - Board.Wait calls endWait with what it returned. A seq it returned is
//     settled for every wait of the task on that topic; a seq it did not
//     return, and no other live wait still owes, makes endWait report that
//     the withheld wake is due, and Wait emits it.
//
// Deciding after the message became visible lost this both ways: a wait could
// take the message and end before the check, which then woke the task too;
// and a check keyed on the topic alone withheld the wake for a message the
// wait's in_reply_to filter would never return.
//
// since and inReplyTo never change after beginWait; owed is guarded by the
// taskState's mu. Every method here keeps that mu a leaf lock (nothing else is
// taken under it): Send takes it under the topic's lock.
type liveWait struct {
	since, inReplyTo uint64
	owed             []uint64
}

// wants reports whether this wait returns m to a reader that is task self.
// Board.Wait filters its scan with it, so handOff predicts exactly that.
func (w *liveWait) wants(m RetainedMessage, self protocol.TaskID) bool {
	return m.Seq > w.since && !m.ownExcluded(self) &&
		(w.inReplyTo == 0 || m.InReplyTo == w.inReplyTo)
}

// beginWait registers a live synchronous wait on topic. Paired with endWait.
func (t *taskState) beginWait(topic string, since, inReplyTo uint64) *liveWait {
	w := &liveWait{since: since, inReplyTo: inReplyTo}
	t.mu.Lock()
	t.waits[topic] = append(t.waits[topic], w)
	t.mu.Unlock()
	return w
}

// handOff reports whether a live wait on topic will return m, recording it as
// owed by each such wait. True means the publish emits no wake for this task.
func (t *taskState) handOff(topic string, m RetainedMessage) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	handed := false
	for _, w := range t.waits[topic] {
		if w.wants(m, t.tid) {
			w.owed = append(w.owed, m.Seq)
			handed = true
		}
	}
	return handed
}

// endWait retires w, which returned took, and reports whether a wake handOff
// withheld is now due.
func (t *taskState) endWait(topic string, w *liveWait, took []RetainedMessage) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	rest := make([]*liveWait, 0, len(t.waits[topic]))
	for _, o := range t.waits[topic] {
		if o != w {
			rest = append(rest, o)
		}
	}
	if len(rest) == 0 {
		delete(t.waits, topic)
	} else {
		t.waits[topic] = rest
	}
	due := false
	for _, seq := range w.owed {
		if returned(took, seq) {
			for _, o := range rest {
				o.owed = slices.DeleteFunc(o.owed, func(s uint64) bool { return s == seq })
			}
			continue
		}
		if !slices.ContainsFunc(rest, func(o *liveWait) bool { return slices.Contains(o.owed, seq) }) {
			due = true
		}
	}
	return due
}

func returned(took []RetainedMessage, seq uint64) bool {
	return slices.ContainsFunc(took, func(m RetainedMessage) bool { return m.Seq == seq })
}

// isWaiting reports whether this task has a live synchronous wait on topic.
func (t *taskState) isWaiting(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.waits[topic]
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
	_, ok := t.waits[topic]
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
	out := make([]string, 0, len(t.patterns)+len(t.waits))
	for p := range t.patterns {
		out = append(out, p)
	}
	for p := range t.waits {
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
