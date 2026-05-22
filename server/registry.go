package server

import (
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerEntry holds the current state of a connected runner.
//
// Read methods (Get, List, Candidates) return value snapshots; callers
// may freely read the returned values. All mutations go through the
// Add / Remove / BindTask / UnbindTask / SetLastSeen methods.
//
// Conn is set by the server when registering and is the path through which
// sendAssign reaches the runner. The value-snapshot semantics still hold
// (the field is a copy of an interface value). Conn may be nil if the entry
// was constructed without an active connection (e.g. in tests).
//
// ActiveTasks is a set of task IDs (hex strings) currently bound to this
// runner. len(ActiveTasks) is the current load; capacity is MaxTasks.
type RunnerEntry struct {
	ID           string              // = objproto.ConnectionID.String()
	Hostname     string              // from RunnerHello.hostname
	AllowedRoots []string            // POSIX '/'-paths, path.Clean'd at Hello receipt (wire-format)
	MaxTasks     int                 // from RunnerHello.max_tasks (>=1)
	ActiveTasks  map[string]struct{} // task_id (hex) set; len() = current load
	ConnectedAt  time.Time
	LastSeen     time.Time
	Conn         ConnHandle // set by server.go on registration; nil in zero-value / test stubs
}

// Status derives the wire-visible status from connection + slot occupancy.
// Offline = no Conn; Busy = at capacity; Idle = capacity remains.
func (e *RunnerEntry) Status() protocol.RunnerStatus {
	if e.Conn == nil {
		return protocol.RunnerStatus_Offline
	}
	if len(e.ActiveTasks) >= e.MaxTasks {
		return protocol.RunnerStatus_Busy
	}
	return protocol.RunnerStatus_Idle
}

// Registry tracks connected runners. All public methods are concurrency-safe.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]*RunnerEntry

	OnAdd    func(RunnerEntry)               // optional; called after Add inserts an entry.
	OnRemove func(id string, snap RunnerEntry) // optional; called after Remove deletes an entry.
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		runners: make(map[string]*RunnerEntry),
	}
}

// Add inserts or replaces the entry keyed by e.ID.
func (r *Registry) Add(e *RunnerEntry) {
	r.mu.Lock()
	// Ensure ActiveTasks is initialized.
	if e.ActiveTasks == nil {
		e.ActiveTasks = make(map[string]struct{})
	}
	r.runners[e.ID] = e
	snapshot := *e
	onAdd := r.OnAdd
	r.mu.Unlock()
	if onAdd != nil {
		onAdd(snapshot)
	}
}

// Remove deletes the entry with the given id. No-op if absent.
// The snapshot of the entry at removal time is passed to OnRemove so the
// callback can inspect which tasks were stranded.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	e, existed := r.runners[id]
	var snap RunnerEntry
	if existed {
		snap = *e
	}
	delete(r.runners, id)
	onRemove := r.OnRemove
	r.mu.Unlock()
	if existed && onRemove != nil {
		onRemove(id, snap)
	}
}

// Get returns a value snapshot of the entry for id. The returned value is
// independent of the internal map; callers may read or copy it freely.
func (r *Registry) Get(id string) (RunnerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.runners[id]
	if !ok {
		return RunnerEntry{}, false
	}
	return *e, true
}

// SetLastSeen updates the runner's LastSeen timestamp to ts.
// Returns false if the runner is not registered.
func (r *Registry) SetLastSeen(id string, ts time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return false
	}
	e.LastSeen = ts
	return true
}

// BindTask atomically reserves a task slot on the runner. Returns false if
// the runner is unknown or already at capacity. Caller (dispatcher) must
// call UnbindTask on send failure to roll back the reservation.
func (r *Registry) BindTask(id, taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return false
	}
	if len(e.ActiveTasks) >= e.MaxTasks {
		return false
	}
	if e.ActiveTasks == nil {
		e.ActiveTasks = make(map[string]struct{})
	}
	e.ActiveTasks[taskID] = struct{}{}
	e.LastSeen = time.Now()
	return true
}

// UnbindTask releases a previously-reserved slot. Idempotent: no error if the
// runner is unknown or did not hold the task. This makes it safe to call
// from both the dispatcher's rollback path and the runner_handler's
// TaskFinished path even if they race.
func (r *Registry) UnbindTask(id, taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return
	}
	delete(e.ActiveTasks, taskID)
	e.LastSeen = time.Now()
}

// Candidates returns runner snapshots that can serve repo, restricted to
// the most specific tier of matching roots (longest-prefix-match) and
// further filtered by the selector (or any runner if Kind == Any).
//
// Specificity per runner is the length of its longest matching root
// (protocol.MatchLen). Across selector-matched runners, only those whose
// score equals the global maximum survive — so a focused per-repo runner
// shadows a broad fallback runner that would otherwise also match.
//
// The slice is capacity-agnostic: at-capacity runners are still listed so
// callers can detect ambiguity even when matching runners are all busy.
//
// The result is sorted by ConnectedAt asc then ID asc for deterministic
// behavior in tests and dispatch.
func (r *Registry) Candidates(repo string, sel protocol.RunnerSelector) []RunnerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type scored struct {
		entry RunnerEntry
		score int
	}
	var matches []scored
	maxScore := 0
	for _, e := range r.runners {
		if !selectorMatches(sel, e) {
			continue
		}
		score := bestRootScore(e.AllowedRoots, repo)
		if score == 0 {
			continue
		}
		if score > maxScore {
			maxScore = score
		}
		matches = append(matches, scored{entry: *e, score: score})
	}
	var out []RunnerEntry
	for _, m := range matches {
		if m.score == maxScore {
			out = append(out, m.entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ConnectedAt.Before(out[j].ConnectedAt)
	})
	return out
}

// bestRootScore returns the largest protocol.MatchLen across roots, or 0 if
// none contain repo.
func bestRootScore(roots []string, repo string) int {
	best := 0
	for _, root := range roots {
		if s := protocol.MatchLen(root, repo); s > best {
			best = s
		}
	}
	return best
}

func selectorMatches(sel protocol.RunnerSelector, e *RunnerEntry) bool {
	switch sel.Kind {
	case protocol.RunnerSelectorKind_Any:
		return true
	case protocol.RunnerSelectorKind_ByRunnerId:
		want := sel.RunnerId()
		return want != nil && runnerIDMatches(e.ID, want)
	case protocol.RunnerSelectorKind_ByHostname:
		h := sel.Hostname()
		return h != nil && string(h.Name) == e.Hostname
	case protocol.RunnerSelectorKind_ByIp:
		ip := sel.IpAddr()
		return ip != nil && runnerIDIPMatches(e.ID, ip.Addr)
	}
	return false
}

// runnerIDMatches checks if a protocol.RunnerID matches a ConnectionID string.
// Matches when transport, IP, port, and UniqueNumber all equal.
func runnerIDMatches(id string, rid *protocol.RunnerID) bool {
	if rid == nil {
		return false
	}
	cid, err := objproto.ParseConnectionID(id, 0)
	if err != nil {
		return false
	}
	if string(rid.Transport) != cid.Transport {
		return false
	}
	gotIP := cid.Addr.Addr().AsSlice()
	if len(gotIP) != len(rid.IpAddr) {
		return false
	}
	for i := range gotIP {
		if gotIP[i] != rid.IpAddr[i] {
			return false
		}
	}
	if uint16(cid.Addr.Port()) != rid.Port {
		return false
	}
	return uint16(cid.ID) == rid.UniqueNumber
}

// runnerIDIPMatches extracts the IP bytes from a ConnectionID-encoded ID string
// and compares to want. Format: "transport:ip:port-id", e.g. "ws:127.0.0.1:8539-1".
func runnerIDIPMatches(id string, want []byte) bool {
	addr, err := parseConnIDForIP(id)
	if err != nil {
		return false
	}
	got := addr.AsSlice()
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// parseConnIDForIP parses the IP address component from a ConnectionID string.
func parseConnIDForIP(id string) (netip.Addr, error) {
	cid, err := objproto.ParseConnectionID(id, 0)
	if err != nil {
		return netip.Addr{}, err
	}
	return cid.Addr.Addr(), nil
}

// GetByConnectionID returns a pointer to the registered RunnerEntry whose
// stringified ConnectionID matches cid, or nil/false on no match.
//
// Unlike Get (which keys by the same canonical string but returns a value
// snapshot), this accessor returns the live pointer so the via-relay path
// can read the live ConnHandle and Addr without an extra lookup. The map
// stores *RunnerEntry, so the returned pointer is the same one mutations
// race against — callers must treat it as read-only.
//
// Used by the dial-runner via-relay path (DialRunnerHandler.ResolveVia) to
// resolve a CLI-supplied via=<cid> against the live registered runners.
func (r *Registry) GetByConnectionID(cid objproto.ConnectionID) (*RunnerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.runners[cid.String()]
	if !ok {
		return nil, false
	}
	return entry, true
}

// List returns value snapshots of all entries in arbitrary order.
// The returned slice is independent of the internal map.
func (r *Registry) List() []RunnerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]RunnerEntry, 0, len(r.runners))
	for _, e := range r.runners {
		result = append(result, *e)
	}
	return result
}
