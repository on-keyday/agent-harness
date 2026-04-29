package agentboard

import "sync"

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
	conns    map[*ConnState]struct{}
}

func newTaskState() *taskState {
	return &taskState{
		patterns: make(map[string]struct{}),
		conns:    make(map[*ConnState]struct{}),
	}
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

func (t *taskState) matches(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.patterns[topic]
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

func (t *taskState) snapshotPatterns() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.patterns))
	for p := range t.patterns {
		out = append(out, p)
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
